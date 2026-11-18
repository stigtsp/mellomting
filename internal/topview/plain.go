package topview

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"mellomting/internal/adminapi"
)

// Plain writes one reply as text without escape sequences, for --once
// and for output that is not a terminal. Every string from outside is
// sanitized, and redacted when asked.
func Plain(w io.Writer, st adminapi.Stats, opt Options) {
	m := NewModel(opt.Loc)
	m.Apply(st)
	ui := NewUI(opt)
	show := func(s string) string { return m.show(&ui, s) }
	g := &m.global
	reason, _ := m.recentRefusal(10)
	// Colour, when on, marks only what to look at: the title, section
	// headings and attention lines. Tables stay plain so they align.
	paint := func(sgr, s string) string {
		if !opt.Color {
			return s
		}
		return "\x1b[" + sgr + "m" + s + "\x1b[0m"
	}
	heading := func(s string) string { return paint("1", s) }
	fmt.Fprintf(w, "%s %s  %d in flight", paint("1;38;5;75", "mellomting"), m.now().Format("15:04:05"), len(st.Requests))
	if opt.Redact {
		fmt.Fprint(w, " "+paint("1;38;5;214", "REDACTED"))
	}
	fmt.Fprintf(w, "  inflight %d/%d  offered %s/s  completed %s/s  errors %s/s  refused %s/s",
		st.Global.Inflight, st.Global.MaxInflight, num(m.rate(g, offered)), num(m.rate(g, completed)), num(m.rate(g, failed)), num(m.rate(g, refused)))
	if reason != "" {
		fmt.Fprintf(w, " (%s)", reason)
	}
	fmt.Fprintf(w, "  tok/s %s\n\n%s\n", num(m.rate(g, tokOut)), heading("ATTENTION"))
	for _, a := range attention(m) {
		sgr := "38;5;114"
		switch a.st.FG {
		case bad.FG:
			sgr = "38;5;203"
		case warn.FG:
			sgr = "38;5;214"
		case 0:
			sgr = "2"
		}
		fmt.Fprintf(w, "  %s %s\n", paint(sgr, "●"), show(a.text))
	}

	table := func(title string, cols []col, rows []row, limit int) {
		fmt.Fprintf(w, "\n%s\n", heading(title))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		var heads []string
		var keep []int
		for i, c := range cols {
			if c.title == "LAST 5M" {
				continue
			}
			heads = append(heads, c.title)
			keep = append(keep, i)
		}
		fmt.Fprintln(tw, strings.Join(heads, "\t"))
		for n, r := range rows {
			if limit > 0 && n >= limit {
				fmt.Fprintf(tw, "… %d more\n", len(rows)-n)
				break
			}
			var cells []string
			for _, i := range keep {
				t := "-"
				if i < len(r.cells) && r.cells[i].text != "" {
					t = r.cells[i].text
				}
				cells = append(cells, strings.ReplaceAll(show(t), "\t", " "))
			}
			fmt.Fprintln(tw, strings.Join(cells, "\t"))
		}
		tw.Flush()
	}
	for _, v := range []View{Keys, Backends, Models} {
		u := ui
		u.View = v
		table(strings.ToUpper(v.String()), columns(v), viewRows(m, &u), 0)
	}
	u := ui
	u.View = Requests
	table("REQUESTS", columns(Requests), viewRows(m, &u), opt.Rows)
}
