// Package guest holds the build configuration of herd's one Linux guest: the same kernel configuration and
// initramfs list for arm64 and amd64, booted by Cloud Hypervisor on Ubuntu and by Virtualization.framework on macOS
// (spec R-D8). The Go code here only checks that configuration; build.sh builds it.
package guest
