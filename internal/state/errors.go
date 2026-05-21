package state

import "errors"

var (
	ErrUsernameTaken = errors.New("username taken")
	ErrUserNotFound  = errors.New("user not found")
	ErrRoomNotFound  = errors.New("room not found")
	ErrRoomFull      = errors.New("room full")
	ErrAlreadyInRoom = errors.New("user already in a room")
)
