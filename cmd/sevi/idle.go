package main

import "sevi/internal/idle"

// newIdle builds the idle manager from config (idle.*, all optional with
// defaults), or returns nil in dev mode (app.dev), where the server runs until
// stopped.
func (a *app) newIdle() *idle.Manager {
	if a.Cfg.GetBool("app.dev") {
		return nil
	}
	return idle.New(idle.Config{
		Initial:     a.Minutes("idle.initial_min"),
		BlogWindow:  a.Minutes("idle.blog_min"),
		FirstAnswer: a.Minutes("idle.first_answer_min"),
		Extend:      a.Minutes("idle.extend_min"),
		Cap:         a.Minutes("idle.cap_min"),
	}, a.Log)
}
