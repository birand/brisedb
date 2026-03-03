// volume-server is a standalone HTTP blob storage server.
// It accepts writes (POST /write) and reads (GET /read) over HTTP and
// stores blobs in local volume files on one or more drives.
//
// Usage:
//
//	./volume-server --addr :8081 --data /mnt/ssd1/volumes [--data /mnt/ssd2/volumes]
package main

import (
	"flag"
	"log/slog"
	"os"
	"strings"

	"github.com/birand/brisedb/pkg/brisedb"
	"github.com/birand/brisedb/pkg/volumeserver"
)

func main() {
	addr := flag.String("addr", ":8081", "HTTP listen address")
	dataFlag := flag.String("data", "vol-data", "comma-separated list of drive directories")
	maxSize := flag.Uint64("max-size", 0, "max volume file size in bytes (0 = 2 GiB)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	drives := strings.Split(*dataFlag, ",")
	for i, d := range drives {
		drives[i] = strings.TrimSpace(d)
	}

	vm, err := brisedb.NewVolumeManager(drives, *maxSize)
	if err != nil {
		log.Error("failed to open volume manager", "error", err)
		os.Exit(1)
	}
	defer vm.Close()

	backend := &volumeserver.FuncBackend{
		WriteFn: func(data []byte) (volumeserver.WriteResult, error) {
			addr, err := vm.Write(data)
			if err != nil {
				return volumeserver.WriteResult{}, err
			}
			return volumeserver.WriteResult{
				VolumeID: addr.VolumeID,
				Offset:   addr.Offset,
				Size:     addr.Size,
			}, nil
		},
		ReadFn: func(volID uint32, offset, size uint64) ([]byte, error) {
			return vm.Read(brisedb.NeedleAddr{VolumeID: volID, Offset: offset, Size: size})
		},
		StatsFn: func() []volumeserver.VolumeStat {
			infos := vm.Stats()
			stats := make([]volumeserver.VolumeStat, len(infos))
			for i, info := range infos {
				stats[i] = volumeserver.VolumeStat{
					VolumeID: info.VolumeID,
					Drive:    info.Drive,
					Path:     info.Path,
					Size:     info.Size,
				}
			}
			return stats
		},
	}

	srv := volumeserver.New(backend, log)
	if err := srv.ListenAndServe(*addr); err != nil {
		log.Error("volume server error", "error", err)
		os.Exit(1)
	}
}
