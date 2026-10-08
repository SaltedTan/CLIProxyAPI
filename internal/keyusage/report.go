// Package keyusage tells the holder of a client API key how much of the key's Claude
// allowance is left and when its window resets, and how much Fable allowance is
// left across the Claude accounts that serve Fable. It shows the key holder their
// own key only, and Fable as one combined figure that names no account.
package keyusage

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clientusage"
)

// Report is what the holder of a client key may see.
type Report struct {
	GeneratedAt time.Time                `json:"generated_at"`
	Key         Key                      `json:"key"`
	Claude      clientusage.KeyAllowance `json:"claude"`
	Fable       FableSummary             `json:"fable"`
}

// Key identifies the client key a report is about by its ID and display name; the
// key itself is never echoed.
type Key struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// Build reports on the client key apiKey, whose display name is name. It waits up to
// fableWait for Fable accounts that have never been read.
func Build(ctx context.Context, apiKey, name string, tracker *clientusage.Tracker, pool *FablePool, fableWait time.Duration) Report {
	return Report{
		GeneratedAt: time.Now(),
		Key:         Key{ID: clientusage.KeyID(apiKey), Name: strings.TrimSpace(name)},
		Claude:      tracker.KeyAllowance(apiKey),
		Fable:       pool.Summary(ctx, fableWait),
	}
}

// KeyName resolves the display name of a client key from access.api-key-names, which
// is keyed by full key or key ID.
func KeyName(names map[string]string, apiKey string) string {
	if name := strings.TrimSpace(names[apiKey]); name != "" && apiKey != "" {
		return name
	}
	return strings.TrimSpace(names[clientusage.KeyID(apiKey)])
}

// Text renders the report for a terminal. Times are shown as UTC and as the time
// left, since the server's time zone is not the reader's.
func (r Report) Text() string {
	var b strings.Builder
	key := r.Key.ID
	if r.Key.Name != "" {
		key = fmt.Sprintf("%s (%s)", r.Key.Name, r.Key.ID)
	}
	fmt.Fprintf(&b, "Client key: %s\n\n", key)

	claude := r.Claude
	switch {
	case !claude.Limited:
		fmt.Fprintf(&b, "Claude allowance: no limit on this key (%s Pro units used in the current window)\n", clientusage.FormatProUnits(claude.UsedProUnits))
	case claude.LimitReached:
		fmt.Fprintf(&b, "Claude allowance: limit reached, %s of %s Pro units used\n", clientusage.FormatProUnits(claude.UsedProUnits), clientusage.FormatProUnits(*claude.LimitProUnits))
	default:
		fmt.Fprintf(&b, "Claude allowance: %s%% left, %s of %s Pro units used\n", formatPercent(*claude.RemainingPercent), clientusage.FormatProUnits(claude.UsedProUnits), clientusage.FormatProUnits(*claude.LimitProUnits))
	}
	if claude.WindowResetsAt != nil {
		fmt.Fprintf(&b, "  Window resets in %s (%s)\n", r.until(*claude.WindowResetsAt), formatUTC(*claude.WindowResetsAt))
	} else {
		b.WriteString("  No window running: your next Claude request starts a 7-day window.\n")
	}

	b.WriteString("\n")
	fable := r.Fable
	if !fable.Available {
		b.WriteString("Fable (shared): usage unavailable right now\n")
		return b.String()
	}
	fmt.Fprintf(&b, "Fable (shared by all keys): %s%% left\n", formatPercent(fable.RemainingPercent))
	if fable.NextResetAt != nil {
		fmt.Fprintf(&b, "  Next top-up in %s (%s): +%s%%\n", r.until(*fable.NextResetAt), formatUTC(*fable.NextResetAt), formatPercent(fable.NextResetRestoresPercent))
	}
	if fable.Partial {
		b.WriteString("  Some accounts could not be read; the figure may be off.\n")
	}
	return b.String()
}

func (r Report) until(at time.Time) string {
	return clientusage.FormatResetDuration(at.Sub(r.GeneratedAt))
}

func formatUTC(at time.Time) string {
	return at.UTC().Format("Mon 2006-01-02 15:04 UTC")
}

func formatPercent(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// ANSI colors for Line: green while more than half is left, yellow down to a fifth,
// red below.
const (
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiRed    = "\x1b[31m"
	ansiReset  = "\x1b[0m"
)

// Line renders the report as one short line for a status bar such as Claude Code's,
// with times as the time left. color adds ANSI colors to the percentages left.
func (r Report) Line(color bool) string {
	paint := func(percent float64, text string) string {
		if !color {
			return text
		}
		code := ansiGreen
		switch {
		case percent < 20:
			code = ansiRed
		case percent <= 50:
			code = ansiYellow
		}
		return code + text + ansiReset
	}

	claude := r.Claude
	var parts []string
	switch {
	case !claude.Limited:
		parts = append(parts, "Claude: no limit")
	case claude.LimitReached:
		parts = append(parts, paint(0, "Claude: limit reached")+" · back in "+r.until(*claude.WindowResetsAt))
	case claude.WindowResetsAt == nil:
		parts = append(parts, paint(100, "Claude 100% left")+" · 7d window starts on next use")
	default:
		parts = append(parts, paint(*claude.RemainingPercent, "Claude "+wholePercent(*claude.RemainingPercent)+"% left")+" · resets in "+r.until(*claude.WindowResetsAt))
	}

	fable := r.Fable
	switch {
	case !fable.Available:
		parts = append(parts, "Fable: n/a")
	default:
		text := paint(fable.RemainingPercent, "Fable "+wholePercent(fable.RemainingPercent)+"% left")
		if fable.Partial {
			text += " (partial)"
		}
		if fable.NextResetAt != nil {
			text += " · +" + wholePercent(fable.NextResetRestoresPercent) + "% in " + r.until(*fable.NextResetAt)
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, " │ ")
}

// wholePercent rounds a percentage for Line, keeping a small nonzero one visible.
func wholePercent(value float64) string {
	if value > 0 && value < 1 {
		return "<1"
	}
	return formatPercent(math.Round(value))
}
