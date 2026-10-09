package syncer

import (
	"testing"

	"courier/internal/provider/apk/apktest"
)

func apkKeys(t *testing.T) (apktest.Key, string) { return apktest.NewKey(t) }
