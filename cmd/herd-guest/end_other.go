//go:build !linux

package main

import "errors"

func endVM() error { return errors.New("herd-guest runs as PID 1 of a Linux microVM only") }
