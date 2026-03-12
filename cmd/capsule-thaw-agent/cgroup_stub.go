//go:build !linux

package main

import "syscall"

type cgroupManager struct{}

func initCgroup() *cgroupManager { return nil }

func (cm *cgroupManager) applyCgroup(attr *syscall.SysProcAttr) {}
