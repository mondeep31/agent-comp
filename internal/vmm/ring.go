package vmm

import "sync"

// Ring is an io.Writer that keeps only the last N bytes written. It bounds the
// memory a chatty guest console can use.
type Ring struct {
	mu   sync.Mutex
	buf  []byte
	size int
}

func NewRing(size int) *Ring {
	return &Ring{buf: make([]byte, 0, size), size: size}
}

func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(p)
	if n >= r.size {
		r.buf = append(r.buf[:0], p[n-r.size:]...)
		return n, nil
	}
	if over := len(r.buf) + n - r.size; over > 0 {
		r.buf = append(r.buf[:0], r.buf[over:]...)
	}
	r.buf = append(r.buf, p...)
	return n, nil
}

// Bytes returns a copy of the retained output.
func (r *Ring) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]byte, len(r.buf))
	copy(out, r.buf)
	return out
}
