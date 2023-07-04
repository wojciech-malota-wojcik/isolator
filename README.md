# Isolator

The goal of this library is to run shell commands in isolation using linux namespaces.
It may be called from any other program requiring to run some operation
inside container.

The goal of this library is not to be compliant with opencontainers spec. It rather provides
functionality required in my other projects.

## How to use it

Take a look at [examples](examples)

## Features

- library may be used by other software instantly, it doesn't depend on starting another instance of `/proc/self/exe` like other libraries do,
- root permissions are not required to run a container,
- runs commands inside `PID`, `NS`, `USER`, `IPC` and `UTS` namespaces. `NET` namespace is not used to make an internet available to container instantly,
- communication is done in JSON format using stdin and stdout as transport layer,
- logs printed by executed command are transmitted back to the caller,
- `/proc` is mounted inside container and populated with in-container processes,
- `/dev` is populated with basic devices: `null`, `zero`, `random`, `urandom` by binding them to those existing on host,
- `tmpfs` is mounted on `/tmp`,
- DNS inside container is set to `8.8.8.8` and `8.8.4.4` by populating `/etc/resolv.conf`,
- library supports mounting custom locations inside container (mounts may be writable or read-only).