package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	queueGroup     = "workers"
	maxStreamLen   = 10000
	readBlockTime  = time.Second // also bounds how long a shutdown waits for an idle worker
	redisOpTimeout = 5 * time.Second
)

// transitionScript changes a job's status only while it still has the expected one, so a
// cancel and a worker start cannot both win. KEYS[1] job; ARGV: from, to, job JSON, TTL in ms
// (0 = keep).
var transitionScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'status') ~= ARGV[1] then return 0 end
redis.call('HSET', KEYS[1], 'status', ARGV[2], 'data', ARGV[3])
if tonumber(ARGV[4]) > 0 then redis.call('PEXPIRE', KEYS[1], ARGV[4]) end
return 1
`)

// store keeps everything that must be shared between replicas in Redis:
//
//	<prefix>:job:<id>  hash (status, data = Job JSON); expires JobRetention after the job finished
//	<prefix>:jobs      sorted set of job ids by creation time
//	<prefix>:queue     stream of job ids waiting for a worker, read through a consumer group
//	<prefix>:events    pub/sub channel carrying every job event to all replicas
//	<prefix>:cancel    pub/sub channel carrying the id of a job to cancel to all replicas
type store struct {
	rdb       *redis.Client
	prefix    string
	retention time.Duration
	consumer  string
}

func newStore(rdb *redis.Client, cfg *Config) *store {
	host, _ := os.Hostname()
	return &store{
		rdb: rdb, prefix: cfg.RedisKeyPrefix, retention: cfg.JobRetention,
		consumer: host + "-" + newID()[:8],
	}
}

func (s *store) jobKey(id string) string { return s.prefix + ":job:" + id }
func (s *store) jobsKey() string         { return s.prefix + ":jobs" }
func (s *store) streamKey() string       { return s.prefix + ":queue" }
func (s *store) eventsChannel() string   { return s.prefix + ":events" }
func (s *store) cancelChannel() string   { return s.prefix + ":cancel" }

func (s *store) ttlMs(status string) int64 {
	if isTerminal(status) {
		return max(s.retention.Milliseconds(), 1)
	}
	return 0
}

// create stores a new job and appends it to the queue in one transaction.
func (s *store) create(ctx context.Context, j *Job) error {
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	_, err = s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.HSet(ctx, s.jobKey(j.ID), "status", j.Status, "data", data)
		p.ZAdd(ctx, s.jobsKey(), redis.Z{Score: float64(j.CreatedAt.UnixMilli()), Member: j.ID})
		p.XAdd(ctx, &redis.XAddArgs{
			Stream: s.streamKey(), MaxLen: maxStreamLen, Approx: true, Values: map[string]any{"job": j.ID},
		})
		return nil
	})
	return err
}

// save overwrites the state of a job. Only the replica running the job calls it.
func (s *store) save(ctx context.Context, j *Job) error {
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	_, err = s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.HSet(ctx, s.jobKey(j.ID), "status", j.Status, "data", data)
		// Re-indexes the job if Redis lost its data in the meantime.
		p.ZAddNX(ctx, s.jobsKey(), redis.Z{Score: float64(j.CreatedAt.UnixMilli()), Member: j.ID})
		if ttl := s.ttlMs(j.Status); ttl > 0 {
			p.PExpire(ctx, s.jobKey(j.ID), time.Duration(ttl)*time.Millisecond)
		}
		return nil
	})
	return err
}

// transition saves j only if the stored status is still from. It reports whether it did.
func (s *store) transition(ctx context.Context, j *Job, from string) (bool, error) {
	data, err := json.Marshal(j)
	if err != nil {
		return false, err
	}
	n, err := transitionScript.Run(ctx, s.rdb, []string{s.jobKey(j.ID)},
		from, j.Status, string(data), s.ttlMs(j.Status)).Int()
	return n == 1, err
}

func (s *store) get(ctx context.Context, id string) (Job, bool, error) {
	data, err := s.rdb.HGet(ctx, s.jobKey(id), "data").Result()
	if errors.Is(err, redis.Nil) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	var j Job
	if err := json.Unmarshal([]byte(data), &j); err != nil {
		return Job{}, false, err
	}
	return j, true, nil
}

// list returns all retained jobs, newest first. Index entries of expired jobs are removed.
func (s *store) list(ctx context.Context) ([]Job, error) {
	ids, err := s.rdb.ZRevRange(ctx, s.jobsKey(), 0, -1).Result()
	if err != nil || len(ids) == 0 {
		return []Job{}, err
	}
	cmds := make([]*redis.StringCmd, len(ids))
	_, err = s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, id := range ids {
			cmds[i] = p.HGet(ctx, s.jobKey(id), "data")
		}
		return nil
	})
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	jobs := make([]Job, 0, len(ids))
	var gone []any
	for i, c := range cmds {
		data, err := c.Result()
		if errors.Is(err, redis.Nil) {
			gone = append(gone, ids[i])
			continue
		}
		var j Job
		if err != nil || json.Unmarshal([]byte(data), &j) != nil {
			continue
		}
		jobs = append(jobs, j)
	}
	if len(gone) > 0 {
		s.rdb.ZRem(ctx, s.jobsKey(), gone...)
	}
	return jobs, nil
}

func (s *store) publishEvent(ctx context.Context, ev Event) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	return s.rdb.Publish(ctx, s.eventsChannel(), data).Err()
}

func (s *store) publishCancel(ctx context.Context, id string) error {
	return s.rdb.Publish(ctx, s.cancelChannel(), id).Err()
}

// ensureGroup creates the queue and its consumer group when they do not exist yet, which
// is also the case after Redis lost its data.
func (s *store) ensureGroup(ctx context.Context) error {
	err := s.rdb.XGroupCreateMkStream(ctx, s.streamKey(), queueGroup, "0").Err()
	if err != nil && !strings.HasPrefix(err.Error(), "BUSYGROUP") {
		return err
	}
	return nil
}

// queued is one entry of the queue.
type queued struct {
	msgID string
	jobID string
}

// next returns the next job to work on: first an entry another replica left unfinished
// for at least claimIdle (it died), otherwise a new one, waiting up to readBlockTime.
func (s *store) next(ctx context.Context, claimIdle time.Duration) (queued, bool, error) {
	msgs, _, err := s.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream: s.streamKey(), Group: queueGroup, Consumer: s.consumer,
		MinIdle: claimIdle, Start: "0", Count: 1,
	}).Result()
	if err != nil {
		return queued{}, false, err
	}
	if len(msgs) == 0 {
		streams, err := s.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group: queueGroup, Consumer: s.consumer,
			Streams: []string{s.streamKey(), ">"}, Count: 1, Block: readBlockTime,
		}).Result()
		if errors.Is(err, redis.Nil) {
			return queued{}, false, nil
		}
		if err != nil {
			return queued{}, false, err
		}
		for _, st := range streams {
			msgs = append(msgs, st.Messages...)
		}
	}
	if len(msgs) == 0 {
		return queued{}, false, nil
	}
	id, _ := msgs[0].Values["job"].(string)
	return queued{msgID: msgs[0].ID, jobID: id}, true, nil
}

// touch resets the idle time of an entry this replica holds, so that the others do not take
// it over while the job is still running.
func (s *store) touch(ctx context.Context, msgID string) error {
	return s.rdb.XClaim(ctx, &redis.XClaimArgs{
		Stream: s.streamKey(), Group: queueGroup, Consumer: s.consumer, Messages: []string{msgID},
	}).Err()
}

func (s *store) ack(ctx context.Context, msgID string) error {
	if err := s.rdb.XAck(ctx, s.streamKey(), queueGroup, msgID).Err(); err != nil {
		return err
	}
	return s.rdb.XDel(ctx, s.streamKey(), msgID).Err()
}
