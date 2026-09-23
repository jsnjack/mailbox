package syncer

import "sync"

func (e *Engine) syncMuFor(accountID int64) *sync.Mutex {
	e.syncMuMu.Lock()
	defer e.syncMuMu.Unlock()
	if e.syncMus == nil {
		e.syncMus = make(map[int64]*sync.Mutex)
	}
	if e.syncMus[accountID] == nil {
		e.syncMus[accountID] = new(sync.Mutex)
	}
	return e.syncMus[accountID]
}
