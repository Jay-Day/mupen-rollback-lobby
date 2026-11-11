package state

import "errors"

var (
	ErrUsernameTaken = errors.New("username taken")
	ErrUserNotFound  = errors.New("user not found")
	ErrRoomNotFound  = errors.New("room not found")
	ErrRoomFull      = errors.New("room full")
	// ErrNotEnoughSeats: the room has free seats, but fewer than the players
	// asking to sit down together.
	ErrNotEnoughSeats = errors.New("not enough seats")
	ErrAlreadyInRoom  = errors.New("user already in a room")
)
