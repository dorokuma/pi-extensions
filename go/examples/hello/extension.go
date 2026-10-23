package hello

import (
	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

func Extension() *sdk.Extension {
	e := sdk.New("hello")

	e.Tool("hello_ping", "Return a short pong so you can confirm the extension is wired up.",
		sdk.Schema{"type": "object", "properties": map[string]any{}},
		func(ctx sdk.Context, params map[string]any) (any, error) {
			return "pong from hello", nil
		})

	e.Command("hello", "Say hello from the hello extension.", func(ctx sdk.Context, args string) error {
		ctx.Notify("hello from hello", "info")
		return nil
	})

	e.OnSessionStart(func(ctx sdk.Context, _ map[string]any) (any, error) {
		ctx.SetStatus("hello", "hello loaded")
		return nil, nil
	})

	return e
}
