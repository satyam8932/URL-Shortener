// Package buildinfo exposes metadata stamped into the binary at link time.
package buildinfo

// Version identifies the running build. It defaults to "dev" and is set for
// release builds with:
//
//	go build -ldflags "-X url_shortener/internal/buildinfo.Version=v1.2.3"
var Version = "dev"
