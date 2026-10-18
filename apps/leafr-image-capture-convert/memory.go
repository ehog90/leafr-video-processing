package main

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
)

func logMemoryStats(source, phase string) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	rss, err := processRSS()
	if err != nil {
		log.Printf("🧠 capture source=%s memory %s: process RSS unavailable (%v); Go heap alloc=%s in-use=%s sys=%s released=%s",
			source, phase, err, memorySize(int64(mem.HeapAlloc)), memorySize(int64(mem.HeapInuse)),
			memorySize(int64(mem.HeapSys)), memorySize(int64(mem.HeapReleased)))
		return
	}
	log.Printf("🧠 capture source=%s memory %s: process RSS=%s; Go heap alloc=%s in-use=%s sys=%s released=%s",
		source, phase, memorySize(int64(rss)), memorySize(int64(mem.HeapAlloc)), memorySize(int64(mem.HeapInuse)),
		memorySize(int64(mem.HeapSys)), memorySize(int64(mem.HeapReleased)))
}

func processRSS() (uint64, error) {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(status), "\n") {
		if value, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			fields := strings.Fields(value)
			if len(fields) < 2 || fields[1] != "kB" {
				return 0, fmt.Errorf("unexpected VmRSS format")
			}
			kb, err := strconv.ParseUint(fields[0], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse VmRSS: %w", err)
			}
			return kb * 1024, nil
		}
	}
	return 0, fmt.Errorf("VmRSS not found in /proc/self/status")
}

func memorySize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
