package organize

// OrderBookMoves exposes orderBookMoves to the external tests.
func OrderBookMoves(acts []Action) []Action { return orderBookMoves(acts) }
