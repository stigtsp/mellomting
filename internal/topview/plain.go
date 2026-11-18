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
	fmt.Fprintf(w, "mellomting %s", m.now().Format("15:04:05"))
	if opt.Redact {
		fmt.Fprint(w, " REDACTED")
	}
	fmt.Fprintf(w, "  inflight %d/%d  offered %s/s  completed %s/s  errors %s/s  refused %s/s",
		st.Global.Inflight, st.Global.MaxInflight, num(m.rate(g, offered)), num(m.rate(g, completed)), num(m.rate(g, failed)), num(m.rate(g, refused)))
	if reason != "" {
		fmt.Fprintf(w, " (%s)", reason)
	}
	fmt.Fprintf(w, "  tok/s %s\n\nATTENTION\n", num(m.rate(g, tokOut)))
	for _, a := range attention(m) {
		fmt.Fprintf(w, "  %s\n", show(a.text))
	}

	table := func(title string, cols []col, rows []row, limit int) {
		fmt.Fprintf(w, "\n%s\n", title)
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
