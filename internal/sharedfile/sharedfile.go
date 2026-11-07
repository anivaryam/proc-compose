// Package sharedfile opens, reads and replaces files that one process renames,
// rotates or rewrites while other processes read them.
//
// On Unix it is the os package: an open file never stops a rename or a delete.
// On Windows an open handle stops both unless it allows delete sharing, which
// os.Open and os.OpenFile never request, and a rename over a file that is open
// fails even then unless it uses POSIX semantics. A file that is read through
// Open or ReadFile and written through OpenAppend or Replace therefore behaves
// as it does on Unix: a reader never makes a rotation or an atomic replace
// fail, and it keeps reading the file it opened.
package sharedfile
