package num

// Side names a bid in its own YES/NO coordinates, not the wire bid/ask side.
// Sharing it below quote and risk prevents incompatible duplicate types.
type Side uint8

const (
	SideYes Side = iota
	SideNo
)

func (s Side) String() string {
	if s == SideNo {
		return "no"
	}
	return "yes"
}

func (s Side) Opposite() Side {
	if s == SideYes {
		return SideNo
	}
	return SideYes
}

// confidence: high
