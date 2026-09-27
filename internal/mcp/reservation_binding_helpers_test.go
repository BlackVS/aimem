package mcp

import "aimem/internal/store"

// testBinding is a personal holder binding for actor, for tests that seed a
// hold directly in the store.
func testBinding(actor store.TaskActor) store.ReservationBinding {
	return store.ReservationBinding{UserID: actor.UserID, Mode: "personal"}
}

func allowReservation() error { return nil }
