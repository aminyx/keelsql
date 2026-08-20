// Command keelsql is a REPL for a keelsql database.
//
//	keelsql [-db dir] [-c "SQL"] [-echo] [-explain]
//
// With -c it runs one script and exits. Without it, it reads statements
// from standard input — a terminal or a pipe — until end of input.
// Statements end with a semicolon; a statement may span several lines.
//
// Dot commands are handled by the REPL rather than the engine:
//
//	.tables            list the tables
//	.schema [table]    print CREATE TABLE for one table or all of them
//	.explain on|off    print the plan before running each query
//	.timer on|off      print how long each statement took
//	.help              this list
//	.quit              leave
//
// The -echo flag makes the REPL print the input it reads after the prompt,
// so that piping a script through it produces the same transcript an
// interactive session would. That is how the session in the README was
// captured.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aminyx/keelsql"
	"github.com/aminyx/keelsql/ast"
	"github.com/aminyx/keelsql/catalog"
	"github.com/aminyx/keelsql/lexer"
	"github.com/aminyx/keelsql/parser"
)

const version = "0.1.0"

func main() {
	var (
		dir     = flag.String("db", "keeldata", "database directory")
		command = flag.String("c", "", "run this SQL and exit")
		echo    = flag.Bool("echo", false, "echo input after the prompt (for transcripts)")
		explain = flag.Bool("explain", false, "start with .explain on")
		quiet   = flag.Bool("quiet", false, "do not print the banner")
		timer   = flag.Bool("timer", true, "print statement timings")
	)
	flag.Parse()

	db, err := keelsql.Open(*dir, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "keelsql:", err)
		os.Exit(1)
	}
	defer db.Close()

	repl := &repl{
		db:      db,
		conn:    db.Conn(),
		out:     os.Stdout,
		errOut:  os.Stderr,
		echo:    *echo,
		explain: *explain,
		timer:   *timer,
	}

	if *command != "" {
		if err := repl.runText(*command); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		return
	}

	if !*quiet {
		fmt.Fprintf(repl.out, "keelsql %s — a SQL engine on keelstore\n", version)
		fmt.Fprintf(repl.out, "database %s; .help for commands, .quit to leave\n\n", db.Path())
	}
	if err := repl.loop(os.Stdin); err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintln(os.Stderr, "keelsql:", err)
		os.Exit(1)
	}
}

type repl struct {
	db      *keelsql.DB
	conn    *keelsql.Conn
	out     io.Writer
	errOut  io.Writer
	echo    bool
	explain bool
	timer   bool
}

// loop reads statements until the input runs out. A statement is complete
// when the accumulated text ends in a semicolon and the lexer can tokenise
// it — which is how a semicolon inside a string literal fails to end a
// statement.
func (r *repl) loop(in io.Reader) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var pending strings.Builder
	for {
		if pending.Len() == 0 {
			r.prompt("keelsql> ")
		} else {
			r.prompt("    ...> ")
		}
		if !scanner.Scan() {
			break
		}
		line := scanner.Text()
		if r.echo {
			fmt.Fprintln(r.out, line)
		}

		if pending.Len() == 0 {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			if strings.HasPrefix(trimmed, ".") {
				if stop := r.dotCommand(trimmed); stop {
					return nil
				}
				continue
			}
		}
		pending.WriteString(line)
		pending.WriteByte('\n')

		if !complete(pending.String()) {
			continue
		}
		text := pending.String()
		pending.Reset()
		if err := r.runText(text); err != nil {
			fmt.Fprintln(r.errOut, "Error:", err)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if rest := strings.TrimSpace(pending.String()); rest != "" {
		return r.runText(rest)
	}
	return nil
}

func (r *repl) prompt(s string) {
	fmt.Fprint(r.out, s)
}

// complete reports whether the buffered text holds at least one whole
// statement. It tokenises rather than scanning for ';' by hand, so a
// semicolon inside 'a;b' does not end anything, and an unterminated string
// or comment simply asks for another line.
func complete(text string) bool {
	tokens, err := lexer.Tokenize(text)
	if err != nil {
		return false
	}
	for _, tok := range tokens {
		if tok.Is(";") {
			return true
		}
	}
	return false
}

// runText parses and runs every statement in text.
func (r *repl) runText(text string) error {
	stmts, err := parser.ParseMany(text)
	if err != nil {
		return err
	}
	for _, stmt := range stmts {
		if err := r.runStatement(stmt); err != nil {
			return err
		}
	}
	return nil
}

func (r *repl) runStatement(stmt ast.Statement) error {
	if r.explain && wantsPlan(stmt) {
		res, err := r.conn.RunStatement(&ast.Explain{Stmt: stmt})
		if err != nil {
			return err
		}
		fmt.Fprintln(r.out, res.Plan)
	}

	start := time.Now()
	res, err := r.conn.RunStatement(stmt)
	elapsed := time.Since(start)
	if err != nil {
		return err
	}
	r.report(res, elapsed)
	return nil
}

// wantsPlan reports whether .explain on should print a plan for this
// statement. DDL and transaction control have nothing interesting to show.
func wantsPlan(stmt ast.Statement) bool {
	switch stmt.(type) {
	case *ast.Select, *ast.Update, *ast.Delete, *ast.Insert:
		return true
	}
	return false
}

func (r *repl) report(res *keelsql.Result, elapsed time.Duration) {
	if len(res.Columns) > 0 {
		fmt.Fprint(r.out, keelsql.FormatTable(res.Columns, res.Rows))
	}
	summary := res.Tag
	switch res.Tag {
	case "SELECT", "EXPLAIN":
		summary = fmt.Sprintf("%d %s", len(res.Rows), plural(len(res.Rows), "row", "rows"))
	case "INSERT", "UPDATE", "DELETE":
		summary = fmt.Sprintf("%s %d", res.Tag, res.Affected)
	}
	if r.timer {
		fmt.Fprintf(r.out, "-- %s (%s)\n", summary, formatDuration(elapsed))
	} else {
		fmt.Fprintf(r.out, "-- %s\n", summary)
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// formatDuration prints a millisecond figure with three decimals, which is
// fine-grained enough for a point lookup and still readable for a scan.
func formatDuration(d time.Duration) string {
	return fmt.Sprintf("%.3fms", float64(d.Nanoseconds())/1e6)
}

// ---------------------------------------------------------------------
// dot commands
// ---------------------------------------------------------------------

// dotCommand runs a REPL command and reports whether the REPL should stop.
func (r *repl) dotCommand(line string) bool {
	fields := strings.Fields(line)
	switch fields[0] {
	case ".quit", ".exit":
		return true

	case ".help":
		fmt.Fprint(r.out, helpText)

	case ".tables":
		names := r.db.Catalog().Names()
		if len(names) == 0 {
			fmt.Fprintln(r.out, "-- no tables")
			return false
		}
		fmt.Fprintln(r.out, strings.Join(names, "  "))

	case ".schema":
		tables := r.db.Catalog().All()
		if len(fields) > 1 {
			t, err := r.db.Catalog().Get(strings.ToLower(fields[1]))
			if err != nil {
				fmt.Fprintln(r.errOut, "Error:", err)
				return false
			}
			tables = []*catalog.Table{t}
		}
		for _, t := range tables {
			fmt.Fprintln(r.out, t.SQL())
		}

	case ".explain":
		r.explain = onOff(fields, r.explain)
		fmt.Fprintf(r.out, "-- explain %s\n", onOffText(r.explain))

	case ".timer":
		r.timer = onOff(fields, r.timer)
		fmt.Fprintf(r.out, "-- timer %s\n", onOffText(r.timer))

	default:
		fmt.Fprintf(r.errOut, "Error: unknown command %q; .help for the list\n", fields[0])
	}
	return false
}

func onOff(fields []string, current bool) bool {
	if len(fields) < 2 {
		return !current
	}
	switch strings.ToLower(fields[1]) {
	case "on", "true", "1":
		return true
	case "off", "false", "0":
		return false
	}
	return current
}

func onOffText(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

const helpText = `.tables            list the tables
.schema [table]    print the schema of one table, or of all of them
.explain on|off    print the query plan before running each statement
.timer on|off      print how long each statement took
.help              this list
.quit              leave
`
