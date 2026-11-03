#!/bin/sh
# Run in the kernel tree. linux 6.1.191's ext4 mballoc checks __filemap_get_folio()
# with IS_ERR(), but on 6.1 that function returns NULL: a backport from >= 6.3, where
# it returns ERR_PTR. So any ext4 block allocation or free with the group's buddy page
# out of the page cache (drop_caches, memory pressure, balloon) is a NULL dereference
# in ext4_mb_load_buddy_gfp, which is what killed guests restored from a template
# (boat-e2e 41596728088). Give mballoc.c the >= 6.3 contract. Drop this when a 6.1.y
# release has fixed it: the guard below fails the build if the code changes.
set -eu
f=fs/ext4/mballoc.c
grep -q 'IS_ERR(folio)' "$f"
sed -i 's/__filemap_get_folio(/mb_filemap_get_folio(/' "$f"
# First line after the includes.
sed -i '0,/^#include <trace\/events\/ext4.h>/s//#include <trace\/events\/ext4.h>\n\nstatic inline struct folio *mb_filemap_get_folio(struct address_space *m, pgoff_t i, int fgp, gfp_t gfp)\n{\n\tstruct folio *f = __filemap_get_folio(m, i, fgp, gfp);\n\n\treturn f ?: ERR_PTR(-ENOMEM);\n}/' "$f"
grep -q 'static inline struct folio \*mb_filemap_get_folio' "$f"
# The helper's own call must stay the real one.
grep -q 'f = __filemap_get_folio(m, i, fgp, gfp)' "$f"
