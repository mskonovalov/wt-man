//go:build !darwin

package main

import (
	"os"
	"time"
)

func creationTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}
