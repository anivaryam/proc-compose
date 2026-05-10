package ansi

import "testing"

func TestWrap_DefaultEnabled(t *testing.T) {
	SetDisabled(false)
	if got := Wrap(Red); got != Red {
		t.Errorf("Wrap(Red) = %q, want %q", got, Red)
	}
}

func TestWrap_DisabledStripsCodes(t *testing.T) {
	SetDisabled(true)
	defer SetDisabled(false)

	if got := Wrap(Red); got != "" {
		t.Errorf("Wrap(Red) when disabled = %q, want empty", got)
	}
	if got := ProcessColor(0); got != "" {
		t.Errorf("ProcessColor(0) when disabled = %q, want empty", got)
	}
}

func TestProcessColor_NegativeIndexNormalised(t *testing.T) {
	SetDisabled(false)
	if got := ProcessColor(-1); got == "" {
		t.Error("ProcessColor(-1) returned empty; expected normalised colour code")
	}
}

func TestProcessColor_CyclesWithModulo(t *testing.T) {
	SetDisabled(false)
	if a, b := ProcessColor(0), ProcessColor(len(ProcessColors)); a != b {
		t.Errorf("ProcessColor(0)=%q != ProcessColor(N)=%q (expected modulo cycle)", a, b)
	}
}
