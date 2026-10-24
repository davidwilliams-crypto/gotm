// Command notices writes THIRD-PARTY-NOTICES.txt for a Go program: the license
// text of every module compiled into it, plus the Go standard library's.
//
//	go run ./tools/notices -o dist\THIRD-PARTY-NOTICES.txt .
//
// Binary distribution of BSD-licensed code (Go, Walk, x/sys …) requires
// shipping these notices, so build.ps1 runs this for every release.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type module struct {
	Path, Version, Dir string
	Main               bool
	Replace            *module
}

type pkg struct {
	Standard bool
	Module   *module
}

func main() {
	out := flag.String("o", "THIRD-PARTY-NOTICES.txt", "output file")
	app := flag.String("app", "gotm", "program name for the header")
	flag.Parse()
	target := "."
	if flag.NArg() > 0 {
		target = flag.Arg(0)
	}
	if err := run(*out, *app, target); err != nil {
		fmt.Fprintln(os.Stderr, "notices:", err)
		os.Exit(1)
	}
}

func run(out, app, target string) error {
	cmd := exec.Command("go", "list", "-deps", "-json", target)
	cmd.Env = append(os.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0")
	raw, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("go list: %w", err)
	}
	mods := map[string]module{}
	usesStd := false
	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		var p pkg
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		if p.Standard {
			usesStd = true
			continue
		}
		if p.Module == nil || p.Module.Main {
			continue
		}
		m := *p.Module
		if m.Replace != nil {
			m.Dir = m.Replace.Dir
		}
		mods[m.Path] = m
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s includes the following third-party software. Their licenses require that\r\n", app)
	fmt.Fprintf(&b, "these notices accompany every copy of the program.\r\n\r\n")

	if usesStd {
		gr, err := exec.Command("go", "env", "GOROOT").Output()
		if err != nil {
			return err
		}
		ver, _ := exec.Command("go", "env", "GOVERSION").Output()
		text, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(gr)), "LICENSE"))
		if err != nil {
			return fmt.Errorf("Go license: %w", err)
		}
		section(&b, "The Go programming language (standard library, runtime) "+strings.TrimSpace(string(ver)), "https://go.dev", text)
	}

	paths := make([]string, 0, len(mods))
	for p := range mods {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		m := mods[p]
		text, name, err := licenseText(m.Dir)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		section(&b, fmt.Sprintf("%s %s (%s)", m.Path, m.Version, name), "https://"+m.Path, text)
	}
	return os.WriteFile(out, []byte(b.String()), 0o644)
}

// licenseText finds the license file in a module's root folder.
func licenseText(dir string) ([]byte, string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, "", err
	}
	var names []string
	for _, e := range entries {
		n := strings.ToUpper(e.Name())
		if !e.IsDir() && (strings.HasPrefix(n, "LICENSE") || strings.HasPrefix(n, "LICENCE") || strings.HasPrefix(n, "COPYING")) {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return nil, "", fmt.Errorf("no LICENSE file in %s", dir)
	}
	sort.Strings(names)
	var all []byte
	for i, n := range names {
		t, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, "", err
		}
		if i > 0 {
			all = append(all, "\n\n"...)
		}
		all = append(all, t...)
	}
	// Also carry a NOTICE file when the module ships one (Apache-2.0 requires it).
	for _, e := range entries {
		if strings.HasPrefix(strings.ToUpper(e.Name()), "NOTICE") && !e.IsDir() {
			if t, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil {
				all = append(all, "\n\n"...)
				all = append(all, t...)
			}
		}
	}
	return all, strings.Join(names, ", "), nil
}

func section(b *strings.Builder, title, url string, text []byte) {
	b.WriteString(strings.Repeat("=", 78) + "\r\n")
	b.WriteString(title + "\r\n" + url + "\r\n")
	b.WriteString(strings.Repeat("-", 78) + "\r\n")
	t := strings.ReplaceAll(strings.ReplaceAll(string(text), "\r\n", "\n"), "\n", "\r\n")
	b.WriteString(strings.TrimRight(t, "\r\n") + "\r\n\r\n")
}
