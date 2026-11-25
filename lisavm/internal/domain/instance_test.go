package domain

import "testing"

func TestStateTransitions(t *testing.T) {
	valid := [][2]State{
		{StateStopped, StateBooting},
		{StateBooting, StateRunning},
		{StateRunning, StateSleeping},
		{StateSleeping, StateWaking},
		{StateWaking, StateRunning},
		{StateSleeping, StateDeleting},
		{StateRunning, StateDeleting},
	}
	for _, pair := range valid {
		if err := pair[0].ValidateTransition(pair[1]); err != nil {
			t.Fatalf("expected %s -> %s valid: %v", pair[0], pair[1], err)
		}
	}

	invalid := [][2]State{
		{StateStopped, StateSleeping},
		{StateStopped, StateRunning},
		{StateDeleting, StateRunning},
		{StateDeleting, StateStopped},
		{StateRunning, StateWaking},
	}
	for _, pair := range invalid {
		if err := pair[0].ValidateTransition(pair[1]); err == nil {
			t.Fatalf("expected %s -> %s invalid", pair[0], pair[1])
		}
	}
}
