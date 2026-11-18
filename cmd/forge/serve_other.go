//go:build !linux

package main

// startServer uses the portable netpoller transport off Linux.
func startServer() (int, func(), error) {
	return startNetServer()
}
