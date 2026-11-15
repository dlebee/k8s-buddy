//go:build !darwin

package ui

const nativeAvailable = false

func newNative(o Options) Driver { return newHeadless(o) }
