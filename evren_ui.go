package main

import (
	_ "embed"
)

//go:embed evren_ui.html
var embeddedDashboardHTML string

func GetEmbeddedDashboardHTML() string {
	return embeddedDashboardHTML
}
