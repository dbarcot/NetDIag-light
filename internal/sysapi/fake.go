package sysapi

// FakeConsole is a Console for tests.
type FakeConsole struct {
	Count int
	Err   error
}

func (c *FakeConsole) ProcessCount() (int, error) { return c.Count, c.Err }
