// Package diskwim reads and extracts Windows Imaging Format (WIM) and ESD files
// without mounting them via the OS. It parses the WIM resource table,
// reconstructs directory trees from XML metadata, and extracts files directly
// into any Volume implementation using a unified streaming API.
//
// No wimlib, no DISM, no administrator rights required.
package diskwim