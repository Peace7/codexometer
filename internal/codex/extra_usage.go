package codex

import "context"

// UsageSource supplies monitor-only sessions observed from another local agent.
// Its totals must never decrease within a process: on a transient read error a
// source returns its last good snapshot rather than an error.
type UsageSource interface {
	FetchTokenUsage(context.Context) (LiveUsageSnapshot, error)
}

// mergeExtraUsage folds other agents' sessions into the Codex snapshot. Codex
// runtime status and API-equivalent accounting stay Codex-only. A failing
// source is skipped so it cannot take down Codex monitoring.
func mergeExtraUsage(ctx context.Context, usage LiveUsageSnapshot, sources []UsageSource) LiveUsageSnapshot {
	for _, source := range sources {
		if source == nil {
			continue
		}
		extra, err := source.FetchTokenUsage(ctx)
		if err != nil {
			continue
		}
		usage.TotalTokens += extra.TotalTokens
		usage.SessionCount += extra.SessionCount
		if extra.LastActivity.After(usage.LastActivity) {
			usage.LastActivity = extra.LastActivity
		}
		usage.Sessions = append(usage.Sessions, extra.Sessions...)
	}
	return usage
}
