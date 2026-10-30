//go:build !linux

package main

import "errors"

func run() error { return errors.New("vzguest runs only as /init of a Linux guest") }
