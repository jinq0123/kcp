package internal

type DummyWriter struct{}

func NewDummyWriter() *DummyWriter {
	return &DummyWriter{}
}

// Write implements io.Writer.
func (d *DummyWriter) Write(p []byte) (n int, err error) {
	return len(p), nil
}
