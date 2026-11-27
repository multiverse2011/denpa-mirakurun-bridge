// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// denpa の agent/Denpa.Agent/AgentTls.cs (MPL-2.0, https://github.com/danything/denpa) を Go に移したもの。

package atomicfile

import (
	"os"
	"path/filepath"
)

// WritePrivate は書きかけを読ませず、持ち主だけが読み書きできるように書く
func WritePrivate(name string, data []byte) error {
	if err := WriteSynced(name+".tmp", data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(name+".tmp", name); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(name))
}
