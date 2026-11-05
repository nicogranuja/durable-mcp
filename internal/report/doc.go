// Package report builds a GitHub repository report: issue, pull request, and
// commit activity over a time window.
//
// # Aggregation
//
// The report is computed from per-page partial aggregates. Each page of
// issues, pull requests, or commits is reduced to a small summary (counts,
// merge durations, per-week commit counts, and commits per author), and the
// summaries are added up. Adding them is commutative and the final report
// sorts everything it lists, so the result does not depend on the order in
// which pages are combined.
package report
