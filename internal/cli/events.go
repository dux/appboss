package cli

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"

	"dboss/internal/events"
	"dboss/internal/fsutil"
	"dboss/internal/ops"
)

// printEvents renders the answers of `dboss events` as tables.
func (c CLI) printEvents(method string, data any) error {
	writer := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
	switch method {
	case ops.ActionEvents:
		summary := data.(events.Summary)
		totals := map[[2]string]*events.DayCount{}
		for _, day := range summary.Days {
			key := [2]string{day.NS, day.Event}
			total := totals[key]
			if total == nil {
				total = &events.DayCount{NS: day.NS, Event: day.Event}
				totals[key] = total
			}
			total.Count += day.Count
			total.ValueSum += day.ValueSum
			total.Users = max(total.Users, day.Users)
		}
		users := "users (busiest day)"
		if summary.Scanned {
			users = "users"
		}
		fmt.Fprintf(c.Out, "%s .. %s  %d events  %d %s  value %s  %s on disk\n", summary.From, summary.To, summary.Count, summary.Users, users, number(summary.ValueSum), fsutil.HumanBytes(summary.Bytes))
		if len(totals) == 0 {
			fmt.Fprintln(c.Out, "no events in range")
			return nil
		}
		fmt.Fprintln(writer, "NS\tEVENT\tCOUNT\tVALUE")
		rows := slices.Collect(maps.Values(totals))
		slices.SortFunc(rows, func(a, b *events.DayCount) int {
			return cmp.Or(cmp.Compare(b.Count, a.Count), strings.Compare(a.Event, b.Event))
		})
		for _, row := range rows {
			fmt.Fprintf(writer, "%s\t%s\t%d\t%s\n", row.NS, row.Event, row.Count, number(row.ValueSum))
		}
	case ops.ActionEventsLatest:
		rows := data.([]events.Event)
		if len(rows) == 0 {
			fmt.Fprintln(c.Out, "no matching events")
			return nil
		}
		fmt.Fprintln(writer, "TIME\tNS\tEVENT\tWHO\tTAGS\tMSG")
		for _, row := range rows {
			who := cmp.Or(row.UserID, row.AnonID, "-")
			if row.TenantID != "" {
				who += "@" + row.TenantID
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n", row.TS.Local().Format("2006-01-02 15:04:05"), row.NS, row.Event, who, strings.Join(row.Tags, " "), row.Msg)
		}
	case ops.ActionEventsFacets:
		rows := data.([]events.Facet)
		if len(rows) == 0 {
			fmt.Fprintln(c.Out, "no values")
			return nil
		}
		fmt.Fprintln(writer, "VALUE\tCOUNT")
		for _, row := range rows {
			value := row.Value
			if row.Type != "" {
				value += " (" + row.Type + ")"
			}
			fmt.Fprintf(writer, "%s\t%d\n", value, row.Count)
		}
	case ops.ActionEventsViews:
		result := data.(ops.EventViewsResult)
		fmt.Fprintf(c.Out, "files      %s\nviews.sql  %s\n", result.Status.Dir, result.Status.ViewsSQL)
		if result.Status.DuckDB != "" {
			fmt.Fprintf(c.Out, "duckdb     %s\n", result.Status.DuckDB)
		} else {
			fmt.Fprintf(c.Out, "duckdb     %s\n", result.Status.Missing)
		}
		fmt.Fprintln(c.Out)
		fmt.Fprintln(writer, "KIND\tNAME\tSOURCE\tDEFINITION")
		for _, view := range result.Views {
			fmt.Fprintf(writer, "view\t%s\t%s\t%s\n", view.Name, view.Source, view.Filter)
		}
		for _, funnel := range result.Funnels {
			steps := make([]string, len(funnel.Steps))
			for i, step := range funnel.Steps {
				steps[i] = step.Filter
			}
			fmt.Fprintf(writer, "funnel\t%s\t%s\tby %s within %s: %s\n", funnel.Name, funnel.Source, funnel.By, events.FormatDuration(funnel.Window), strings.Join(steps, " -> "))
		}
		for _, name := range result.Shadowed {
			fmt.Fprintf(writer, "-\t%s\tconsole\thidden by the dboss.yaml entry of the same name\n", name)
		}
	case ops.ActionEventsFunnel, ops.ActionEventsQuery:
		result := data.(events.QueryResult)
		if len(result.Columns) == 0 {
			fmt.Fprintf(c.Out, "no rows (%d ms)\n", result.DurationMS)
			return nil
		}
		fmt.Fprintln(writer, strings.ToUpper(strings.Join(result.Columns, "\t")))
		for _, row := range result.Rows {
			cells := make([]string, len(row))
			for i, cell := range row {
				cells[i] = cellText(cell)
			}
			fmt.Fprintln(writer, strings.Join(cells, "\t"))
		}
		if err := writer.Flush(); err != nil {
			return err
		}
		note := ""
		if result.Truncated {
			note = ", truncated"
		}
		fmt.Fprintf(c.Out, "%d rows (%d ms%s)\n", result.RowCount, result.DurationMS, note)
		return nil
	}
	return writer.Flush()
}

func cellText(cell any) string {
	switch value := cell.(type) {
	case nil:
		return "NULL"
	case string:
		return strings.ReplaceAll(value, "\t", " ")
	case map[string]any, []any:
		encoded, _ := json.Marshal(value)
		return string(encoded)
	default:
		return fmt.Sprint(value)
	}
}

func number(value float64) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.2f", value), "0"), ".")
}
