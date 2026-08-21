// Package plugin is the shared toolkit for viti plugins: the interactive
// picker, table/JSON/YAML output, GitHub release checks, the viti shell-out
// helper, and the self-version/upgrade command scaffolding. Plugins import
// it instead of maintaining their own copies.
//
// Compatibility: vitictl is a v0 module and this package makes NO
// compatibility promise between tags. Plugins pin an exact vitictl version
// in go.mod and upgrade deliberately; breaking changes here are called out
// in vitictl release notes.
package plugin
