package sandbox

import (
	"context"
	"fmt"
	"strings"
	"time"

	agentdv1 "github.com/mondeep31/agent-comp/gen/go/agentcomp/agentd/v1"
	"github.com/mondeep31/agent-comp/internal/config"
)

// Computer-use actions. The names and parameters follow the computer-use
// vocabulary agents already speak (screenshot, left_click, type, key, ...),
// so a harness can forward a model's action unchanged.
const (
	ActionScreenshot     = "screenshot"
	ActionZoom           = "zoom"
	ActionCursorPosition = "cursor_position"
	ActionMouseMove      = "mouse_move"
	ActionLeftClick      = "left_click"
	ActionRightClick     = "right_click"
	ActionMiddleClick    = "middle_click"
	ActionDoubleClick    = "double_click"
	ActionTripleClick    = "triple_click"
	ActionLeftClickDrag  = "left_click_drag"
	ActionLeftMouseDown  = "left_mouse_down"
	ActionLeftMouseUp    = "left_mouse_up"
	ActionScroll         = "scroll"
	ActionType           = "type"
	ActionKey            = "key"
	ActionHoldKey        = "hold_key"
	ActionWait           = "wait"
)

// Actions lists every supported action, for schemas and docs.
var Actions = []string{
	ActionScreenshot, ActionZoom, ActionCursorPosition, ActionMouseMove,
	ActionLeftClick, ActionRightClick, ActionMiddleClick, ActionDoubleClick, ActionTripleClick,
	ActionLeftClickDrag, ActionLeftMouseDown, ActionLeftMouseUp,
	ActionScroll, ActionType, ActionKey, ActionHoldKey, ActionWait,
}

const (
	// MaxWait bounds wait and hold_key.
	MaxWait = 60 * time.Second
	// MaxScrollAmount bounds scroll_amount.
	MaxScrollAmount = 100
	// MaxTypeChars bounds type's text; the guest enforces the same cap.
	MaxTypeChars = 4096
	// DefaultSettle is how long an action's screenshot waits for the screen
	// to react.
	DefaultSettle = 300 * time.Millisecond
	// dragSteps moves the pointer in steps so drag targets see motion.
	dragSteps = 8
)

// ScreenshotOptions selects the image format and part of the screen.
type ScreenshotOptions struct {
	// Format is "png" (default) or "jpeg".
	Format string
	// Quality is the JPEG quality, 1-100; 0 means the guest default.
	Quality int
	// Region is [x0, y0, x1, y1]; nil captures the whole screen.
	Region []int
}

// Image is an encoded screenshot.
type Image struct {
	MIMEType string `json:"mime_type"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Data     []byte `json:"data"`
}

// ComputerAction is one computer-use action.
type ComputerAction struct {
	Action string `json:"action"`
	// Coordinate is [x, y] for clicks, moves, scrolls and the end of a drag.
	Coordinate []int `json:"coordinate,omitempty"`
	// StartCoordinate is where left_click_drag starts.
	StartCoordinate []int `json:"start_coordinate,omitempty"`
	// Text is typed text, a key chord ("ctrl+l") or the key to hold.
	Text string `json:"text,omitempty"`
	// ScrollDirection is up, down, left or right; ScrollAmount is in wheel clicks.
	ScrollDirection string `json:"scroll_direction,omitempty"`
	ScrollAmount    int    `json:"scroll_amount,omitempty"`
	// Duration in seconds, for wait and hold_key.
	Duration float64 `json:"duration,omitempty"`
	// Region is [x0, y0, x1, y1] for zoom.
	Region []int `json:"region,omitempty"`
	// Screenshot asks for a screenshot after the action. screenshot and zoom
	// always return one.
	Screenshot bool `json:"screenshot,omitempty"`
	// Format and Quality apply to any returned screenshot.
	Format  string `json:"format,omitempty"`
	Quality int    `json:"quality,omitempty"`
}

// ComputerResult is what an action produced.
type ComputerResult struct {
	CursorX int    `json:"cursor_x"`
	CursorY int    `json:"cursor_y"`
	Image   *Image `json:"image,omitempty"`
}

// display returns the sandbox's screen, or ErrInvalid for headless images.
func (m *Manager) display(owner, id string) (*config.Display, error) {
	info, err := m.Get(owner, id)
	if err != nil {
		return nil, err
	}
	if info.Display == nil {
		return nil, invalid("sandbox %s has no display; create it from an image with one", id)
	}
	return info.Display, nil
}

// Screenshot captures a sandbox's screen.
func (m *Manager) Screenshot(ctx context.Context, owner, id string, opts ScreenshotOptions) (*Image, error) {
	d, err := m.display(owner, id)
	if err != nil {
		return nil, err
	}
	req, err := screenshotRequest(opts, d)
	if err != nil {
		return nil, err
	}
	c, done, err := m.Use(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	defer done()
	info, data, err := c.Screenshot(ctx, req)
	if err != nil {
		return nil, guestErr(err)
	}
	mime := "image/png"
	if info.GetFormat() == agentdv1.ImageFormat_IMAGE_FORMAT_JPEG {
		mime = "image/jpeg"
	}
	return &Image{MIMEType: mime, Width: int(info.GetWidth()), Height: int(info.GetHeight()), Data: data}, nil
}

func screenshotRequest(opts ScreenshotOptions, d *config.Display) (*agentdv1.Screenshot, error) {
	req := &agentdv1.Screenshot{}
	switch strings.ToLower(opts.Format) {
	case "", "png":
		req.Format = agentdv1.ImageFormat_IMAGE_FORMAT_PNG
	case "jpeg", "jpg":
		req.Format = agentdv1.ImageFormat_IMAGE_FORMAT_JPEG
	default:
		return nil, invalid("format must be png or jpeg")
	}
	if opts.Quality < 0 || opts.Quality > 100 {
		return nil, invalid("quality must be in [1, 100]")
	}
	req.Quality = uint32(opts.Quality) //nolint:gosec // checked above
	if opts.Region != nil {
		r, err := region(opts.Region, d)
		if err != nil {
			return nil, err
		}
		req.Region = r
	}
	return req, nil
}

func region(r []int, d *config.Display) (*agentdv1.Region, error) {
	if len(r) != 4 {
		return nil, invalid("region must be [x0, y0, x1, y1]")
	}
	x0, y0, x1, y1 := r[0], r[1], r[2], r[3]
	if x0 < 0 || y0 < 0 || x1 <= x0 || y1 <= y0 || x1 > d.Width || y1 > d.Height {
		return nil, invalid("region must lie within the %dx%d screen with x0 < x1 and y0 < y1", d.Width, d.Height)
	}
	return &agentdv1.Region{
		X: uint32(x0), Y: uint32(y0), //nolint:gosec // checked above
		Width: uint32(x1 - x0), Height: uint32(y1 - y0), //nolint:gosec // checked above
	}, nil
}

// Computer runs one computer-use action.
func (m *Manager) Computer(ctx context.Context, owner, id string, a *ComputerAction) (*ComputerResult, error) {
	d, err := m.display(owner, id)
	if err != nil {
		return nil, err
	}
	events, wait, err := compile(a, d)
	if err != nil {
		return nil, err
	}
	shotOpts := ScreenshotOptions{Format: a.Format, Quality: a.Quality}
	if a.Action == ActionZoom {
		shotOpts.Region = a.Region
	}
	wantShot := a.Screenshot || a.Action == ActionScreenshot || a.Action == ActionZoom
	if wantShot {
		// Validate before acting, so a bad format does not half-apply an action.
		if _, err := screenshotRequest(shotOpts, d); err != nil {
			return nil, err
		}
	}

	c, done, err := m.Use(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	defer done()
	res, err := c.Input(ctx, &agentdv1.Input{Events: events})
	if err != nil {
		return nil, guestErr(err)
	}
	if wait > 0 {
		if err := sleepCtx(ctx, wait); err != nil {
			return nil, err
		}
	}
	out := &ComputerResult{CursorX: int(res.GetCursorX()), CursorY: int(res.GetCursorY())}
	if !wantShot {
		return out, nil
	}
	if len(events) > 0 && wait == 0 {
		if err := sleepCtx(ctx, DefaultSettle); err != nil {
			return nil, err
		}
	}
	req, _ := screenshotRequest(shotOpts, d)
	info, data, err := c.Screenshot(ctx, req)
	if err != nil {
		return nil, guestErr(err)
	}
	mime := "image/png"
	if info.GetFormat() == agentdv1.ImageFormat_IMAGE_FORMAT_JPEG {
		mime = "image/jpeg"
	}
	out.Image = &Image{MIMEType: mime, Width: int(info.GetWidth()), Height: int(info.GetHeight()), Data: data}
	return out, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func point(c []int, d *config.Display, name string) (int32, int32, error) {
	if len(c) != 2 {
		return 0, 0, invalid("%s must be [x, y]", name)
	}
	if c[0] < 0 || c[1] < 0 || c[0] >= d.Width || c[1] >= d.Height {
		return 0, 0, invalid("%s [%d, %d] is outside the %dx%d screen", name, c[0], c[1], d.Width, d.Height)
	}
	return int32(c[0]), int32(c[1]), nil //nolint:gosec // bounded by the display size
}

func moveEv(x, y int32) *agentdv1.InputEvent {
	return &agentdv1.InputEvent{Kind: &agentdv1.InputEvent_Move{Move: &agentdv1.PointerMove{X: x, Y: y}}}
}

func buttonEv(b agentdv1.Button, a agentdv1.Action, count uint32) *agentdv1.InputEvent {
	return &agentdv1.InputEvent{Kind: &agentdv1.InputEvent_Button{Button: &agentdv1.PointerButton{Button: b, Action: a, Count: count}}}
}

func keyEv(keys string, a agentdv1.Action) *agentdv1.InputEvent {
	return &agentdv1.InputEvent{Kind: &agentdv1.InputEvent_Key{Key: &agentdv1.Key{Keys: keys, Action: a}}}
}

func delayEv(d time.Duration) *agentdv1.InputEvent {
	return &agentdv1.InputEvent{Kind: &agentdv1.InputEvent_Delay{Delay: &agentdv1.Delay{Ms: uint32(d.Milliseconds())}}} //nolint:gosec // bounded by MaxWait
}

func duration(secs float64, name string) (time.Duration, error) {
	d := time.Duration(secs * float64(time.Second))
	if secs <= 0 || d > MaxWait {
		return 0, invalid("%s duration must be in (0, %v] seconds", name, MaxWait.Seconds())
	}
	return d, nil
}

// compile turns an action into guest input events, plus a host-side wait.
func compile(a *ComputerAction, d *config.Display) ([]*agentdv1.InputEvent, time.Duration, error) {
	var evs []*agentdv1.InputEvent
	// An optional coordinate moves the pointer before a click or scroll.
	moveFirst := func() error {
		if a.Coordinate == nil {
			return nil
		}
		x, y, err := point(a.Coordinate, d, "coordinate")
		if err != nil {
			return err
		}
		evs = append(evs, moveEv(x, y))
		return nil
	}
	click := func(b agentdv1.Button, count uint32) error {
		if err := moveFirst(); err != nil {
			return err
		}
		// text holds modifiers to keep down during the click, e.g. "shift".
		if a.Text != "" {
			evs = append(evs, keyEv(a.Text, agentdv1.Action_ACTION_PRESS))
		}
		evs = append(evs, buttonEv(b, agentdv1.Action_ACTION_TAP, count))
		if a.Text != "" {
			evs = append(evs, keyEv(a.Text, agentdv1.Action_ACTION_RELEASE))
		}
		return nil
	}

	var err error
	var wait time.Duration
	switch a.Action {
	case ActionScreenshot, ActionCursorPosition:
	case ActionZoom:
		if a.Region == nil {
			return nil, 0, invalid("zoom needs region [x0, y0, x1, y1]")
		}
		_, err = region(a.Region, d)
	case ActionMouseMove:
		if a.Coordinate == nil {
			return nil, 0, invalid("mouse_move needs coordinate")
		}
		err = moveFirst()
	case ActionLeftClick:
		err = click(agentdv1.Button_BUTTON_LEFT, 1)
	case ActionRightClick:
		err = click(agentdv1.Button_BUTTON_RIGHT, 1)
	case ActionMiddleClick:
		err = click(agentdv1.Button_BUTTON_MIDDLE, 1)
	case ActionDoubleClick:
		err = click(agentdv1.Button_BUTTON_LEFT, 2)
	case ActionTripleClick:
		err = click(agentdv1.Button_BUTTON_LEFT, 3)
	case ActionLeftMouseDown, ActionLeftMouseUp:
		if err = moveFirst(); err == nil {
			act := agentdv1.Action_ACTION_PRESS
			if a.Action == ActionLeftMouseUp {
				act = agentdv1.Action_ACTION_RELEASE
			}
			evs = append(evs, buttonEv(agentdv1.Button_BUTTON_LEFT, act, 0))
		}
	case ActionLeftClickDrag:
		var sx, sy, ex, ey int32
		if sx, sy, err = point(a.StartCoordinate, d, "start_coordinate"); err != nil {
			break
		}
		if ex, ey, err = point(a.Coordinate, d, "coordinate"); err != nil {
			break
		}
		evs = append(evs, moveEv(sx, sy), buttonEv(agentdv1.Button_BUTTON_LEFT, agentdv1.Action_ACTION_PRESS, 0))
		for i := int32(1); i <= dragSteps; i++ {
			evs = append(evs, delayEv(10*time.Millisecond), moveEv(sx+(ex-sx)*i/dragSteps, sy+(ey-sy)*i/dragSteps))
		}
		evs = append(evs, buttonEv(agentdv1.Button_BUTTON_LEFT, agentdv1.Action_ACTION_RELEASE, 0))
	case ActionScroll:
		n := a.ScrollAmount
		if n == 0 {
			n = 3
		}
		if n < 1 || n > MaxScrollAmount {
			return nil, 0, invalid("scroll_amount must be in [1, %d]", MaxScrollAmount)
		}
		var dx, dy int32
		switch a.ScrollDirection {
		case "up":
			dy = -int32(n) //nolint:gosec // bounded above
		case "down":
			dy = int32(n) //nolint:gosec // bounded above
		case "left":
			dx = -int32(n) //nolint:gosec // bounded above
		case "right":
			dx = int32(n) //nolint:gosec // bounded above
		default:
			return nil, 0, invalid("scroll_direction must be up, down, left or right")
		}
		if err = moveFirst(); err == nil {
			if a.Text != "" {
				evs = append(evs, keyEv(a.Text, agentdv1.Action_ACTION_PRESS))
			}
			evs = append(evs, &agentdv1.InputEvent{Kind: &agentdv1.InputEvent_Scroll{Scroll: &agentdv1.Scroll{Dx: dx, Dy: dy}}})
			if a.Text != "" {
				evs = append(evs, keyEv(a.Text, agentdv1.Action_ACTION_RELEASE))
			}
		}
	case ActionType:
		switch n := len([]rune(a.Text)); {
		case n == 0:
			return nil, 0, invalid("type needs text")
		case n > MaxTypeChars:
			return nil, 0, invalid("type text is at most %d characters", MaxTypeChars)
		}
		evs = append(evs, &agentdv1.InputEvent{Kind: &agentdv1.InputEvent_Text{Text: &agentdv1.Text{Text: a.Text}}})
	case ActionKey:
		if strings.TrimSpace(a.Text) == "" {
			return nil, 0, invalid("key needs text, e.g. \"Return\" or \"ctrl+l\"")
		}
		evs = append(evs, keyEv(a.Text, agentdv1.Action_ACTION_TAP))
	case ActionHoldKey:
		if strings.TrimSpace(a.Text) == "" {
			return nil, 0, invalid("hold_key needs text")
		}
		var hold time.Duration
		if hold, err = duration(a.Duration, "hold_key"); err == nil {
			// The guest caps one batch's delay at 30 s.
			if hold > 30*time.Second {
				return nil, 0, invalid("hold_key duration is at most 30 seconds")
			}
			evs = append(evs, keyEv(a.Text, agentdv1.Action_ACTION_PRESS), delayEv(hold), keyEv(a.Text, agentdv1.Action_ACTION_RELEASE))
		}
	case ActionWait:
		wait, err = duration(a.Duration, "wait")
	case "":
		return nil, 0, invalid("action is required")
	default:
		return nil, 0, invalid("unknown action %q; one of %s", a.Action, strings.Join(Actions, ", "))
	}
	if err != nil {
		return nil, 0, err
	}
	return evs, wait, nil
}

// String describes an action for logs.
func (a *ComputerAction) String() string {
	return fmt.Sprintf("%s %v", a.Action, a.Coordinate)
}
