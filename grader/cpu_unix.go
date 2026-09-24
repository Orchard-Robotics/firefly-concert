//go:build !windows

package main

import "syscall"

// cpuSeconds reports the grader's own user+system CPU time, to show whether the grader
// itself was busy enough to limit the measurement.
func cpuSeconds() float64 {
	var u syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &u) != nil {
		return 0
	}
	return float64(u.Utime.Nano()+u.Stime.Nano()) / 1e9
}
