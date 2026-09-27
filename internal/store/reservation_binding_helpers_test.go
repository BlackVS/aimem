package store

// testBinding is a personal holder binding for actor, for store tests that
// stand in for the hub's authorizer.
func testBinding(actor TaskActor) ReservationBinding {
	id := actor.UserID
	if id == "" {
		id = "admin/" + actor.Name
	}
	return ReservationBinding{UserID: id, Mode: "personal"}
}

func allowReservation() error { return nil }
