"""create -> prompt -> fork -> stop. Needs BOAT_URL and BOAT_API_KEY (or `boat login`)."""
import sys

from boat import Boat

boat = Boat()
vm = boat.create_vm(name="demo", size="default", ttl_s=4 * 3600)
print("created", vm.id, vm.state)

for ev in boat.prompt(vm.id, "Create hello.py that prints the date, then run it", agent="claude"):
    if ev.data:
        (sys.stderr if ev.stream == "stderr" else sys.stdout).buffer.write(ev.data)
    if ev.exit_code is not None:
        print("\nagent exited", ev.exit_code)

fork = boat.fork(vm.id, name="demo-try-2")
print("forked", fork.id)
boat.stop(vm.id)
boat.stop(fork.id)
print("stopped both; resume with boat.start(id)")
