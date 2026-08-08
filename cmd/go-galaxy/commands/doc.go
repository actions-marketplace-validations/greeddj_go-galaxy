// Package commands defines the urfave/cli commands go-galaxy exposes:
// install, cleanup, lock, warm, hash, tree, explain, and outdated. Each one
// declares its flag set from cmd/go-galaxy/cliflags and hands the parsed
// command to an entry point under internal/galaxy, so what lives here is the
// wiring rather than the work.
//
// install, cleanup, lock, warm and outdated share one body,
// runCollectionCommand: it builds the *config.Config, creates the progress
// printer and points the standard log package at it, and assembles the
// *infra.Infra every subsystem below is threaded with. hash, tree and explain
// work straight from the lockfile and the requirements file on disk and need
// none of it.
package commands
