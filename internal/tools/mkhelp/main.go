// Command mkhelp prints the documented targets of a Makefile.
//
// It exists so `make help` works identically on Windows and Unix. The usual
// one-line sed or awk incantation depends on tools that are not present in
// cmd.exe, and this is invoked with `go run`, so it needs no installation.
//
// It reads lines of the form:
//
//	## target: description
//	## continuation text
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

type target struct {
	name  string
	short string
	extra []string
}

func main() {
	path := "Makefile"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}

	targets, err := parse(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkhelp: %v\n", err)
		os.Exit(1)
	}

	width := 0
	for _, t := range targets {
		width = max(width, len(t.name))
	}

	fmt.Println("targets:")
	for _, t := range targets {
		fmt.Printf("  %-*s  %s\n", width, t.name, t.short)
		for _, line := range t.extra {
			fmt.Printf("  %-*s  %s\n", width, "", line)
		}
	}
}

func parse(path string) ([]target, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var (
		targets []target
		current *target
	)

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "##") {
			// A blank comment run ends the current entry, so unrelated
			// comments later in the file are not appended to it.
			current = nil
			continue
		}

		body := strings.TrimSpace(strings.TrimPrefix(line, "##"))
		name, desc, isHeader := strings.Cut(body, ":")

		switch {
		case isHeader && current == nil:
			targets = append(targets, target{
				name:  strings.TrimSpace(name),
				short: strings.TrimSpace(desc),
			})
			current = &targets[len(targets)-1]
		case current != nil && body != "":
			current.extra = append(current.extra, body)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return targets, nil
}
