package integrations

import "os"

func nativeGuardOpenFlags() int { return os.O_RDONLY }
