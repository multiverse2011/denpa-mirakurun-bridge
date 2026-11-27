// Package atomicfile は、電源断でも書きかけや空のファイルを残さないようにファイルを書く。
package atomicfile

import "os"

// WriteSynced は rename の前に中身をディスクへ落とし、電源断で空の channels.json が残らないようにする
func WriteSynced(name string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	// 書きかけが前から残っていると OpenFile の perm は効かない
	err = f.Chmod(perm)
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// SyncDir は rename もディスクへ落とす。落とさないと 200 を返した保存が電源断で前の中身に戻りうる
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}
