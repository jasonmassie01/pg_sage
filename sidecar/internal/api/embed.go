package api

import "embed"

//go:embed dist/*
var staticFiles embed.FS
