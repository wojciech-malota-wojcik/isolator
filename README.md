# Isolator

The goal of this library is to run shell commands in isolation using linux namespaces.
It may be called from any other program requiring to run some operation
inside container.

## How to use it

Take a look at [example/main.go](example/main.go)

## Features

- library may be used by other software instantly, it doesn't depend on starting another instance of `/proc/self/exe` like other libraries do,
- runs commands inside `PID`, `NS`, `USER`, `IPC` and `UTS` namespaces. `NET` namespace is not used to make an internet available to container instantly,
- communication is done in JSON format using unix socket, library returns ready to use client,
- logs printed by executed command are transmitted back to the caller,
- `/proc` is mounted inside container,
- `/dev` is populated with basic devices like `null`, `zero`, `random`, `urandom` etc.
- DNS inside container is set to `8.8.8.8` and `8.8.4.4` by populating `/etc/resolv.conf`.