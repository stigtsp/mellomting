// Package landlock implements the post-startup Landlock containment
// (PLAN §53-63): a read-only kernel capability probe (Check), a minimal
// post-startup policy (users-file read, accounting write, backend TCP
// connect ports; everything else denied), and the strict all-thread
// enforcement that the daemon must apply before it accepts any traffic
// (Apply).
//
// Landlock is a Linux-only kernel feature. On other platforms Check
// reports it as unavailable and Apply fails; Mellomting never silently
// degrades a required sandbox to none (PLAN §7, §55).
package landlock
