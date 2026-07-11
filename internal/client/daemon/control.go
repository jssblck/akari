package daemon

import "context"

// ShutdownContext adds the daemon's local control channel to parent. A valid
// stop request cancels the returned context, letting the ordinary watch cleanup
// path release every resource before the process exits.
func (l *Lock) ShutdownContext(parent context.Context) (context.Context, func(), error) {
	ctx, cancel := context.WithCancel(parent)
	control, err := startControl(l.path, l.instance, cancel)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return ctx, func() {
		control.Close()
		cancel()
	}, nil
}
