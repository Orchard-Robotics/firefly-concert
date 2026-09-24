//go:build windows

package main

// cpuSeconds is not measured on Windows; the report shows 0.
func cpuSeconds() float64 { return 0 }
