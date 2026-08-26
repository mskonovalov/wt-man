//go:build !darwin

package main

import "time"

func creationTime(string) time.Time {
	return time.Time{}
}
