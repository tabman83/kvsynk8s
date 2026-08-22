// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

//go:build toolsdeps

// Package azure exists only to hold this build-tag-excluded file, which pins
// the Azure SDK for Go modules in go.mod/go.sum before any real code imports
// them (specs/001-akv-eventgrid-sync T003). Without a live import, `go mod
// tidy` removes an unused module from go.mod entirely, so this file's blank
// imports are what keep the versions resolved and go.sum populated. The
// "toolsdeps" build tag is never set, so this file never compiles into any
// real build or test binary.
//
// azidentity and azsecrets are now pinned for real by keyvault.go (T007), so
// they were dropped from here. Delete this file entirely once the remaining
// packages land: internal/azure/queue.go (azidentity, azqueue/v2) — T017 —
// and internal/events/parser.go (azsystemevents) — T018. At that point their
// own imports keep the dependency pinned and this placeholder is redundant.
package azure

import (
	_ "github.com/Azure/azure-sdk-for-go/sdk/messaging/eventgrid/azsystemevents"
	_ "github.com/Azure/azure-sdk-for-go/sdk/storage/azqueue/v2"
)
