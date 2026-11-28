package sandbox

// runtimeIdentityAfterCreate resolves a just-created runtime directory's
// identity BY PATHNAME, which is a second resolution of the same name and
// therefore a window: the directory created can be renamed away and an ordinary
// one substituted before this runs, and the ledger then records the substitute
// for a directory the run never made.
//
// It is the honest implementation off Windows, where the elevated-installer
// versus unelevated-renamer split this closes does not apply. On Windows the
// creation returns the handle and identity is read from that, so nothing there
// may call this: a Windows test asserts the count is zero, which is what makes
// "the creation establishes the identity" a checked property rather than a
// comment.
var runtimeIdentityAfterCreate = runtimeDirIdentity
