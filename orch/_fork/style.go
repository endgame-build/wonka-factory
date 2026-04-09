package orch

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"
)

var (
	styleSuccess = lipgloss.NewStyle().Foreground(lipgloss.Color("2")) // green
	styleWarning = lipgloss.NewStyle().Foreground(lipgloss.Color("3")) // yellow
	styleError   = lipgloss.NewStyle().Foreground(lipgloss.Color("1")) // red
	styleDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("8")) // gray
	styleBold    = lipgloss.NewStyle().Bold(true)
)

// StatusGlyph returns a styled glyph for a task status.
func StatusGlyph(status TaskStatus) string {
	switch status {
	case StatusCompleted:
		return styleSuccess.Render("✓")
	case StatusFailed:
		return styleError.Render("✗")
	case StatusInProgress:
		return styleWarning.Render("●")
	case StatusAssigned:
		return styleDim.Render("○")
	default:
		return styleDim.Render("·")
	}
}

// PhaseHeader returns a styled phase header line.
func PhaseHeader(phaseName string, index, total int) string {
	return styleBold.Render(fmt.Sprintf("── Phase %d/%d: %s", index+1, total, phaseName))
}

// AgentLine returns a formatted agent status line.
func AgentLine(agentID string, status TaskStatus, output string) string {
	return fmt.Sprintf("  %s %s → %s", StatusGlyph(status), agentID, styleDim.Render(output))
}

// ProgressSummary returns a one-line progress summary.
func ProgressSummary(completed, total, gaps, tolerance int) string {
	summary := fmt.Sprintf("%d/%d tasks", completed, total)
	if gaps > 0 {
		summary += styleWarning.Render(fmt.Sprintf(" (%d/%d gaps)", gaps, tolerance))
	}
	return summary
}

// StyleRetry returns a styled retry/restart prefix for an agent.
func StyleRetry(agentID string) string {
	return styleWarning.Render("↻ " + agentID)
}

// ErrorMessage returns a styled error message.
func ErrorMessage(msg string) string {
	return styleError.Render("✗ " + msg)
}

// SuccessMessage returns a styled success message.
func SuccessMessage(msg string) string {
	return styleSuccess.Render("✓ " + msg)
}
