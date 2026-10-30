// Command checkstatic fails unless every named file is a statically linked ELF executable (no program interpreter, no
// shared library), or a Mach-O executable that loads only libraries macOS itself ships (/usr/lib, /System/Library: libSystem and libresolv for every Go binary, Virtualization.framework for herdlet).
// scripts/release.sh's output is checked with it.
package main

import (
	"debug/elf"
	"debug/macho"
	"fmt"
	"os"
	"strings"
)

func check(path string) error {
	if f, err := elf.Open(path); err == nil {
		defer f.Close()
		for _, p := range f.Progs {
			if p.Type == elf.PT_INTERP {
				return fmt.Errorf("%s: dynamically linked (has a program interpreter)", path)
			}
		}
		if libs, _ := f.ImportedLibraries(); len(libs) > 0 {
			return fmt.Errorf("%s: needs %v", path, libs)
		}
		fmt.Printf("%s: static ELF %v\n", path, f.Machine)
		return nil
	}
	f, err := macho.Open(path)
	if err != nil {
		return fmt.Errorf("%s: neither ELF nor Mach-O: %v", path, err)
	}
	defer f.Close()
	libs, err := f.ImportedLibraries()
	if err != nil {
		return err
	}
	for _, l := range libs {
		if !strings.HasPrefix(l, "/usr/lib/") && !strings.HasPrefix(l, "/System/Library/") {
			return fmt.Errorf("%s: loads %s", path, l)
		}
	}
	fmt.Printf("%s: Mach-O %v loading only system libraries\n", path, f.Cpu)
	return nil
}

func main() {
	bad := 0
	for _, p := range os.Args[1:] {
		if err := check(p); err != nil {
			fmt.Fprintln(os.Stderr, err)
			bad++
		}
	}
	if bad > 0 || len(os.Args) < 2 {
		os.Exit(1)
	}
}
