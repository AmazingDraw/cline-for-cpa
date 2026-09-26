package plugin

import "os"

func isDirectory(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}
