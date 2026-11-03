package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"golang.org/x/term"
)

// ui writes human-oriented output.
type ui struct {
	out   io.Writer
	in    *bufio.Reader
	color bool
	tty   bool
	yes   bool
}

func newUI(yes bool) *ui {
	tty := term.IsTerminal(int(os.Stdout.Fd()))
	enableVT()
	return &ui{
		out:   os.Stdout,
		in:    bufio.NewReader(os.Stdin),
		color: tty && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb",
		tty:   tty && term.IsTerminal(int(os.Stdin.Fd())),
		yes:   yes,
	}
}

func (u *ui) paint(code, s string) string {
	if !u.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (u *ui) bold(s string) string   { return u.paint("1", s) }
func (u *ui) dim(s string) string    { return u.paint("2", s) }
func (u *ui) green(s string) string  { return u.paint("32", s) }
func (u *ui) yellow(s string) string { return u.paint("33", s) }
func (u *ui) red(s string) string    { return u.paint("31", s) }
func (u *ui) cyan(s string) string   { return u.paint("36", s) }

func (u *ui) printf(format string, a ...any) { fmt.Fprintf(u.out, format, a...) }
func (u *ui) println(a ...any)               { fmt.Fprintln(u.out, a...) }

func (u *ui) step(n, total int, title string) {
	u.printf("\n%s %s\n", u.cyan(fmt.Sprintf("[%d/%d]", n, total)), u.bold(title))
}

func (u *ui) ok(format string, a ...any) {
	u.printf("  %s %s\n", u.green("✓"), fmt.Sprintf(format, a...))
}
func (u *ui) warn(format string, a ...any) {
	u.printf("  %s %s\n", u.yellow("!"), fmt.Sprintf(format, a...))
}
func (u *ui) bad(format string, a ...any) {
	u.printf("  %s %s\n", u.red("✗"), fmt.Sprintf(format, a...))
}
func (u *ui) info(format string, a ...any) { u.printf("  %s\n", fmt.Sprintf(format, a...)) }

// ask prompts for a line of input, returning def when empty.
func (u *ui) ask(prompt, def string) string {
	if !u.tty || u.yes {
		return def
	}
	if def != "" {
		u.printf("  %s %s: ", prompt, u.dim("["+def+"]"))
	} else {
		u.printf("  %s: ", prompt)
	}
	line, _ := u.in.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

// secret prompts for hidden input.
func (u *ui) secret(prompt string) string {
	if !u.tty {
		return ""
	}
	u.printf("  %s: ", prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	u.println()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// confirm asks a yes/no question.
func (u *ui) confirm(prompt string, def bool) bool {
	if u.yes {
		return true
	}
	if !u.tty {
		return def
	}
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	u.printf("  %s %s ", prompt, u.dim("["+hint+"]"))
	line, _ := u.in.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	}
	return def
}

// spinner shows activity for long operations on a TTY.
type spinner struct {
	u    *ui
	msg  string
	stop chan struct{}
	wg   sync.WaitGroup
}

func (u *ui) spin(msg string) *spinner {
	s := &spinner{u: u, msg: msg, stop: make(chan struct{})}
	if !u.color {
		u.printf("  %s...\n", msg)
		return s
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		start := time.Now()
		for i := 0; ; i++ {
			select {
			case <-s.stop:
				fmt.Fprintf(u.out, "\r\x1b[2K")
				return
			case <-time.After(100 * time.Millisecond):
				fmt.Fprintf(u.out, "\r\x1b[2K  %s %s %s", u.cyan(frames[i%len(frames)]), msg, u.dim(fmt.Sprintf("(%ds)", int(time.Since(start).Seconds()))))
			}
		}
	}()
	return s
}

func (s *spinner) done() {
	close(s.stop)
	s.wg.Wait()
}

// table renders aligned columns.
func (u *ui) table(header []string, rows [][]string) {
	tw := tabwriter.NewWriter(u.out, 2, 4, 2, ' ', 0)
	h := make([]string, len(header))
	for i, c := range header {
		h[i] = u.bold(c)
	}
	fmt.Fprintln(tw, strings.Join(h, "\t"))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func dur(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func money(v float64) string {
	if v == 0 {
		return "-"
	}
	if v < 0.01 {
		return fmt.Sprintf("$%.4f", v)
	}
	return fmt.Sprintf("$%.2f", v)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
