package mcp

import (
	"os"
	"strings"
)

func NewControllerFromEnv() Controller {
	var base Controller
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("MANTISX_BACKEND_MODE")))
	if mode == "" || mode == "appserver" || mode == "app-server" || mode == "app_server" {
		base = NewAppServerControllerFromEnv()
	} else {
		base = NewRPCControllerFromEnv()
	}
	return NewFailoverControllerFromEnv(base)
}
