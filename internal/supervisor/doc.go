// Package supervisor spawns and supervises the worker process, performs
// periodic health checks, activates new worker versions atomically via
// symlink swap, and rolls back on health failures or repeated crashes.
//
// Populated in T0-05 and T0-10/T0-11.
package supervisor
