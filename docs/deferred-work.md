# Deferred work

Open gaps and their reasons, so a decision to defer something is recorded rather
than rediscovered.

## Live watching holds a descriptor for every file on macOS and the BSDs

fsnotify watches through kqueue there, and its kqueue backend opens a descriptor for
each watched folder and for each file in one, where inotify spends one watch a folder.
A library with more files than the process may hold open cannot be watched whole, and
the watcher cannot count what its source holds. It closes the source (`stop`), which is
the only way the descriptors come back, when it finds the process out of them: at a
watch it adds, at an entry's arrival in a watched folder (fsnotify drops its own error
there, so `handleEvent` looks for a spare descriptor), or in an error the source
reports. `runLive` tries again a few times after a wait, and the scheduled rescans cover
the library meanwhile. A process that runs short between arrivals goes unnoticed until
the next one, and `MaxWatchDirs` counts folders where the cost is in files. Still
missing is a source of our own for these platforms that holds one descriptor a folder,
as `live_windows.go` is for Windows: a folder's own kqueue event says an entry arrived,
left or was renamed, which is all a folder rescan needs, and a file rewritten in place
is the scheduled rescan's to find. It is not written because nothing here runs kqueue.
The tests run on Linux and Windows, in CI and on the development machine, so the event
loop would reach macOS without a test ever having run it. It wants a macOS leg in CI
first.

## The import preflight checks filesystems that share one pool apart

`fsx.Volume` tells volumes apart by device, and btrfs subvolumes, ZFS datasets and the
volumes of an APFS container each report a device of their own over free space they
share. Two roots on one pool are each checked for their own bytes, so an import that
fills the pool between them passes and runs out part way, as it did when every root was
checked alone. No one identity names the pool. The statfs fsid does not: btrfs mixes
the subvolume's id into it so that subvolumes differ, and ZFS and APFS give each dataset
and volume its own. Equal block counts do not either, being what two empty disks of one
size report. Each filesystem names its pool in its own way (btrfs through an ioctl, ZFS
and APFS in the mount source), and none of the three is on the development machine or a
CI runner for a probe to be run against.
