package main

import (
	"bytes"
	"fmt"
	"strings"
)

// diffContext is the number of unchanged lines around a change.
const diffContext = 3

// maxLCS bounds the line comparison table; a larger changed middle is shown
// as one block of removed and added lines.
const maxLCS = 4 << 20

type diffOp struct {
	kind byte // ' ', '-' or '+'
	line string
}

// unifiedDiff returns a unified diff of a and b, empty when they are equal.
// Lines common to the start and the end are matched first, so an appended
// Packages compares in linear time.
func unifiedDiff(name string, a, b []byte) string {
	if bytes.Equal(a, b) {
		return ""
	}
	x, y := splitLines(a), splitLines(b)
	pre := 0
	for pre < len(x) && pre < len(y) && x[pre] == y[pre] {
		pre++
	}
	suf := 0
	for suf < len(x)-pre && suf < len(y)-pre && x[len(x)-1-suf] == y[len(y)-1-suf] {
		suf++
	}
	var ops []diffOp
	for _, l := range x[:pre] {
		ops = append(ops, diffOp{' ', l})
	}
	ops = append(ops, middle(x[pre:len(x)-suf], y[pre:len(y)-suf])...)
	for _, l := range x[len(x)-suf:] {
		ops = append(ops, diffOp{' ', l})
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n", name, name)
	writeHunks(&out, ops)
	return out.String()
}

// splitLines keeps the newline at the end of every line; a last line without
// one gets the diff marker of a missing newline.
func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	lines := strings.SplitAfter(string(b), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if last := lines[len(lines)-1]; !strings.HasSuffix(last, "\n") {
		lines[len(lines)-1] = last + "\n\\ No newline at end of file\n"
	}
	return lines
}

// middle compares the changed part by longest common subsequence.
func middle(x, y []string) []diffOp {
	var ops []diffOp
	if len(x)*len(y) > maxLCS || len(x) == 0 || len(y) == 0 {
		for _, l := range x {
			ops = append(ops, diffOp{'-', l})
		}
		for _, l := range y {
			ops = append(ops, diffOp{'+', l})
		}
		return ops
	}
	n, m := len(x), len(y)
	lcs := make([]int32, (n+1)*(m+1))
	at := func(i, j int) *int32 { return &lcs[i*(m+1)+j] }
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if x[i] == y[j] {
				*at(i, j) = *at(i+1, j+1) + 1
			} else {
				*at(i, j) = max(*at(i+1, j), *at(i, j+1))
			}
		}
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case x[i] == y[j]:
			ops = append(ops, diffOp{' ', x[i]})
			i, j = i+1, j+1
		case *at(i+1, j) >= *at(i, j+1):
			ops = append(ops, diffOp{'-', x[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', y[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', x[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', y[j]})
	}
	return ops
}

// writeHunks groups the changes with diffContext lines around them.
func writeHunks(out *strings.Builder, ops []diffOp) {
	for start := 0; start < len(ops); {
		for start < len(ops) && ops[start].kind == ' ' {
			start++
		}
		if start == len(ops) {
			return
		}
		from := max(start-diffContext, 0)
		end := start
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			run := end
			for run < len(ops) && ops[run].kind == ' ' {
				run++
			}
			if run == len(ops) || run-end > 2*diffContext {
				break
			}
			end = run
		}
		to := min(end+diffContext, len(ops))
		aStart, bStart := 1, 1
		for _, op := range ops[:from] {
			if op.kind != '+' {
				aStart++
			}
			if op.kind != '-' {
				bStart++
			}
		}
		aLen, bLen := 0, 0
		for _, op := range ops[from:to] {
			if op.kind != '+' {
				aLen++
			}
			if op.kind != '-' {
				bLen++
			}
		}
		if aLen == 0 {
			aStart--
		}
		if bLen == 0 {
			bStart--
		}
		fmt.Fprintf(out, "@@ -%d,%d +%d,%d @@\n", aStart, aLen, bStart, bLen)
		for _, op := range ops[from:to] {
			out.WriteByte(op.kind)
			out.WriteString(op.line)
		}
		start = to
	}
}
