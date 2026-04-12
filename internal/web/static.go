package web

import "embed"

// StaticFS holds embedded static UI assets.
//
//go:embed static/*
var StaticFS embed.FS
