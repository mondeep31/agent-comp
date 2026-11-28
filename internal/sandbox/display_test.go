package sandbox

import (
	"strconv"
	"strings"
	"testing"
	"time"

	agentdv1 "github.com/mondeep31/agent-comp/gen/go/agentcomp/agentd/v1"
	"github.com/mondeep31/agent-comp/internal/config"
)

// describe renders events compactly: "move(1,2) click(LEFT,TAP,2) key(ctrl+l,PRESS)".
func describe(evs []*agentdv1.InputEvent) string {
	var parts []string
	for _, e := range evs {
		switch k := e.GetKind().(type) {
		case *agentdv1.InputEvent_Move:
			parts = append(parts, "move("+itoa(k.Move.GetX())+","+itoa(k.Move.GetY())+")")
		case *agentdv1.InputEvent_Button:
			b := strings.TrimPrefix(k.Button.GetButton().String(), "BUTTON_")
			a := strings.TrimPrefix(k.Button.GetAction().String(), "ACTION_")
			parts = append(parts, "button("+b+","+a+","+itoa(int32(k.Button.GetCount()))+")") //nolint:gosec // test
		case *agentdv1.InputEvent_Key:
			parts = append(parts, "key("+k.Key.GetKeys()+","+strings.TrimPrefix(k.Key.GetAction().String(), "ACTION_")+")")
		case *agentdv1.InputEvent_Text:
			parts = append(parts, "text("+k.Text.GetText()+")")
		case *agentdv1.InputEvent_Scroll:
			parts = append(parts, "scroll("+itoa(k.Scroll.GetDx())+","+itoa(k.Scroll.GetDy())+")")
		case *agentdv1.InputEvent_Delay:
			parts = append(parts, "delay("+itoa(int32(k.Delay.GetMs()))+")") //nolint:gosec // test
		}
	}
	return strings.Join(parts, " ")
}

func itoa(n int32) string { return strconv.Itoa(int(n)) }

func TestCompileActions(t *testing.T) {
	d := &config.Display{Width: 1280, Height: 800}
	tests := []struct {
		name    string
		a       ComputerAction
		want    string
		wait    time.Duration
		wantErr string
	}{
		{name: "screenshot", a: ComputerAction{Action: "screenshot"}},
		{name: "cursor", a: ComputerAction{Action: "cursor_position"}},
		{name: "move", a: ComputerAction{Action: "mouse_move", Coordinate: []int{10, 20}}, want: "move(10,20)"},
		{name: "click here", a: ComputerAction{Action: "left_click"}, want: "button(LEFT,TAP,1)"},
		{
			name: "shift click at",
			a:    ComputerAction{Action: "left_click", Coordinate: []int{5, 6}, Text: "shift"},
			want: "move(5,6) key(shift,PRESS) button(LEFT,TAP,1) key(shift,RELEASE)",
		},
		{name: "right", a: ComputerAction{Action: "right_click", Coordinate: []int{1, 1}}, want: "move(1,1) button(RIGHT,TAP,1)"},
		{name: "triple", a: ComputerAction{Action: "triple_click"}, want: "button(LEFT,TAP,3)"},
		{name: "down", a: ComputerAction{Action: "left_mouse_down"}, want: "button(LEFT,PRESS,0)"},
		{name: "up at", a: ComputerAction{Action: "left_mouse_up", Coordinate: []int{3, 4}}, want: "move(3,4) button(LEFT,RELEASE,0)"},
		{name: "scroll", a: ComputerAction{Action: "scroll", ScrollDirection: "up", ScrollAmount: 5}, want: "scroll(0,-5)"},
		{name: "scroll default", a: ComputerAction{Action: "scroll", ScrollDirection: "right"}, want: "scroll(3,0)"},
		{name: "type", a: ComputerAction{Action: "type", Text: "héllo"}, want: "text(héllo)"},
		{name: "key", a: ComputerAction{Action: "key", Text: "ctrl+l"}, want: "key(ctrl+l,TAP)"},
		{
			name: "hold",
			a:    ComputerAction{Action: "hold_key", Text: "shift", Duration: 0.5},
			want: "key(shift,PRESS) delay(500) key(shift,RELEASE)",
		},
		{name: "wait", a: ComputerAction{Action: "wait", Duration: 1.5}, wait: 1500 * time.Millisecond},
		{name: "zoom", a: ComputerAction{Action: "zoom", Region: []int{0, 0, 100, 50}}},

		{name: "no action", a: ComputerAction{}, wantErr: "action is required"},
		{name: "unknown", a: ComputerAction{Action: "dance"}, wantErr: "unknown action"},
		{name: "off screen", a: ComputerAction{Action: "left_click", Coordinate: []int{1280, 0}}, wantErr: "outside"},
		{name: "bad coordinate", a: ComputerAction{Action: "mouse_move", Coordinate: []int{1}}, wantErr: "[x, y]"},
		{name: "move needs coordinate", a: ComputerAction{Action: "mouse_move"}, wantErr: "needs coordinate"},
		{name: "drag needs start", a: ComputerAction{Action: "left_click_drag", Coordinate: []int{1, 1}}, wantErr: "start_coordinate"},
		{name: "scroll direction", a: ComputerAction{Action: "scroll", ScrollDirection: "sideways"}, wantErr: "scroll_direction"},
		{name: "scroll amount", a: ComputerAction{Action: "scroll", ScrollDirection: "up", ScrollAmount: 1000}, wantErr: "scroll_amount"},
		{name: "empty type", a: ComputerAction{Action: "type"}, wantErr: "needs text"},
		{name: "long type", a: ComputerAction{Action: "type", Text: strings.Repeat("x", MaxTypeChars+1)}, wantErr: "at most"},
		{name: "empty key", a: ComputerAction{Action: "key", Text: " "}, wantErr: "needs text"},
		{name: "wait too long", a: ComputerAction{Action: "wait", Duration: 120}, wantErr: "duration"},
		{name: "hold too long", a: ComputerAction{Action: "hold_key", Text: "a", Duration: 45}, wantErr: "30 seconds"},
		{name: "zoom needs region", a: ComputerAction{Action: "zoom"}, wantErr: "region"},
		{name: "zoom inverted", a: ComputerAction{Action: "zoom", Region: []int{100, 0, 50, 50}}, wantErr: "region"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evs, wait, err := compile(&tt.a, d)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := describe(evs); got != tt.want {
				t.Errorf("events = %q, want %q", got, tt.want)
			}
			if wait != tt.wait {
				t.Errorf("wait = %v, want %v", wait, tt.wait)
			}
		})
	}
}

func TestDragMovesInSteps(t *testing.T) {
	d := &config.Display{Width: 1280, Height: 800}
	evs, _, err := compile(&ComputerAction{Action: "left_click_drag", StartCoordinate: []int{0, 0}, Coordinate: []int{80, 40}}, d)
	if err != nil {
		t.Fatal(err)
	}
	got := describe(evs)
	if !strings.HasPrefix(got, "move(0,0) button(LEFT,PRESS,0)") || !strings.HasSuffix(got, "move(80,40) button(LEFT,RELEASE,0)") {
		t.Fatalf("drag = %q", got)
	}
	if n := strings.Count(got, "move("); n != dragSteps+1 {
		t.Fatalf("%d moves, want %d", n, dragSteps+1)
	}
}
