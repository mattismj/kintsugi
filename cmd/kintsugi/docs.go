package main

// kintsugi docs: print the manual for a top-level command.
//
// The pages ship inside the binary (package docs embeds docs/man and
// docs/reference), so `kintsugi docs <command>` works from an installed single-file
// binary with no repository present. Rendering prefers the system man(1) when it
// is available and falls back to a plain-text extraction when it is not.

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"sort"
	"strings"

	"kintsugi/docs"
)

// docsDir reports the directory the embedded pages correspond to. It is kept for
// the tests that walk the shipped tree; the command itself reads the embed, so a
// single-file install still answers.
func docsDir() string {
	for _, dir := range []string{"docs/man", "../docs/man", "../../docs/man"} {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	return ""
}

// manPageName maps a command name to its man page filename.
func manPageName(cmd string) string {
	if cmd == "" || cmd == "kintsugi" {
		return "kintsugi.1"
	}
	return "kintsugi-" + cmd + ".1"
}

// cmdDocs dispatches `kintsugi docs [command] [--md]`.
func cmdDocs(_ string, args []string) {
	var asMD bool
	var rest []string
	for _, a := range args {
		if a == "--md" {
			asMD = true
			continue
		}
		rest = append(rest, a)
	}
	cmd := ""
	if len(rest) > 0 {
		cmd = strings.TrimSpace(strings.TrimPrefix(rest[0], "kintsugi "))
	}
	if cmd == "list" || cmd == "--list" {
		docsList()
		return
	}

	if asMD {
		body, err := docs.Reference.ReadFile("reference/" + referenceName(cmd))
		if err != nil {
			fail("docs: no reference for %q (try `kintsugi docs list`)", cmd)
		}
		fmt.Print(string(body))
		return
	}

	page := manPageName(cmd)
	body, err := docs.Man.ReadFile("man/" + page)
	if err != nil {
		if cmd == "" {
			fail("docs: %s is not in this binary", page)
		}
		fail("docs: no man page for %q (try `kintsugi docs list`)", cmd)
	}

	// Prefer the system man(1) for real roff formatting; fall back to a plain
	// extraction so the page still reads on a box without man installed.
	if renderManPage(body) {
		return
	}
	fmt.Print(roffPageToPlain(string(body)))
}

// renderManPage formats one embedded page through the system man(1). man -l wants
// a real path, so the page is written to a temporary file that keeps its .1
// suffix. It reports whether the page was rendered.
func renderManPage(body []byte) bool {
	if _, err := exec.LookPath("man"); err != nil {
		return false
	}
	tmp, err := os.CreateTemp("", "kintsugi-docs-*.1")
	if err != nil {
		return false
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return false
	}
	if err := tmp.Close(); err != nil {
		return false
	}
	c := exec.Command("man", "-l", tmp.Name())
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run() == nil
}

// referenceName maps a command name to its Markdown reference filename.
func referenceName(cmd string) string {
	if cmd == "" || cmd == "kintsugi" {
		return "kintsugi.md"
	}
	return "kintsugi-" + cmd + ".md"
}

// manPageCommands lists the commands this binary ships a man page for, sorted.
func manPageCommands() []string {
	entries, err := fs.ReadDir(docs.Man, "man")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".1") {
			continue
		}
		cmd := strings.TrimSuffix(name, ".1")
		if cmd != "kintsugi" {
			cmd = strings.TrimPrefix(cmd, "kintsugi-")
		}
		out = append(out, cmd)
	}
	sort.Strings(out)
	return out
}

// docsList prints the manual pages this binary carries.
func docsList() {
	cmds := manPageCommands()
	if len(cmds) == 0 {
		fail("docs: no man pages are embedded in this binary")
	}
	fmt.Println("Available man pages:")
	for _, cmd := range cmds {
		label := cmd
		if cmd == "kintsugi" {
			label = "kintsugi (overview)"
		}
		fmt.Printf("  kintsugi docs %-22s  (%s)\n", label, manPageName(cmd))
	}
	fmt.Println("\nAdd --md for the Markdown reference instead.")
}

// roffPageToPlain extracts readable text from a roff man page, for boxes without
// man(1). It handles the macros the shipped pages use and drops the rest.
func roffPageToPlain(page string) string {
	var b strings.Builder
	for _, line := range strings.Split(page, "\n") {
		if plain := roffToPlain(line); plain != "" {
			b.WriteString(plain)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// roffToPlain converts one roff line to plain text. It handles the most common
// macros found in the shipped man pages.
func roffToPlain(line string) string {
	// Skip comment lines.
	if strings.HasPrefix(line, ".\\\"") {
		return ""
	}
	// Empty macro lines.
	if line == "." {
		return ""
	}
	// Title line: .TH FORGE 1 → FORGE(1).
	if strings.HasPrefix(line, ".TH ") {
		parts := strings.Fields(line)
		if len(parts) >= 3 {
			return fmt.Sprintf("%s(%s)", parts[1], parts[2])
		}
		return ""
	}
	// Section headers: .SH "NAME" → NAME (uppercase).
	if strings.HasPrefix(line, ".SH ") {
		rest := strings.TrimPrefix(line, ".SH ")
		rest = strings.Trim(rest, "\"")
		return "\n" + strings.ToUpper(rest)
	}
	// Font switches: .B, .I, .R — strip the macro, keep the text.
	if strings.HasPrefix(line, ".B ") || strings.HasPrefix(line, ".I ") ||
		strings.HasPrefix(line, ".R ") {
		return line[3:]
	}
	// Indented paragraph with bullet.
	if strings.HasPrefix(line, ".IP \\(bu") {
		return "  •"
	}
	// Indented paragraph: .IP "text" → text.
	if strings.HasPrefix(line, ".IP ") {
		rest := strings.TrimPrefix(line, ".IP ")
		rest = strings.Trim(rest, "\"")
		if rest != "" && rest != "\\(bu" {
			return "  " + rest
		}
		return "  •"
	}
	// Paragraph break.
	if line == ".PP" || line == ".LP" {
		return ""
	}
	// Line break.
	if line == ".br" {
		return ""
	}
	// Skip other macros.
	if strings.HasPrefix(line, ".") {
		return ""
	}
	// Strip inline roff escapes.
	line = strings.ReplaceAll(line, "\\-", "-")
	line = strings.ReplaceAll(line, "\\(bu", "•")
	line = strings.ReplaceAll(line, "\\fB", "")
	line = strings.ReplaceAll(line, "\\fI", "")
	line = strings.ReplaceAll(line, "\\fR", "")
	line = strings.ReplaceAll(line, "\\fP", "")
	return line
}
