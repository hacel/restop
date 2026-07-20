package web

import "embed"

// Assets are compiled into the executable so the UI has no runtime dependencies.
//
//go:embed static/* templates/*
var assets embed.FS
