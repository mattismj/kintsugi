package rank

// Hub is the shared helper every other symbol in this fixture calls.
func Hub() {}

// Client calls Hub.
func Client() {
	Hub()
}

// Server calls Hub.
func Server() {
	Hub()
}
