package engine

import "testing"

func TestHub(t *testing.T) {
	var h hub
	a, releaseA := h.watch("k")
	b, releaseB := h.watch("k")
	other, releaseOther := h.watch("other")
	h.notify("k")
	for _, ch := range []<-chan struct{}{a, b} {
		select {
		case <-ch:
		default:
			t.Fatal("watcher not woken")
		}
	}
	select {
	case <-other:
		t.Fatal("unrelated watcher woken")
	default:
	}
	releaseA()
	releaseB()
	releaseOther()
	releaseOther() // 重复释放无副作用
	if len(h.m) != 0 {
		t.Errorf("leaked watchers: %v", h.m)
	}

	c, releaseC := h.watch("k") // notify 之后的新订阅拿到新的 channel
	select {
	case <-c:
		t.Fatal("new watcher already closed")
	default:
	}
	releaseC()
	if len(h.m) != 0 {
		t.Errorf("leaked watchers after release: %v", h.m)
	}
}
