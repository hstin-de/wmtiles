// thin cgo wrapper over libhdf5; mirrors the wmt_eccodes.h pattern. exposes
// just the surface ODIM_H5 and CF/NetCDF4 readers need.
#ifndef WMT_HDF5_H
#define WMT_HDF5_H

#include <hdf5.h>
#include <stddef.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

// the link-iterator and vlen-reclaim APIs got renamed in HDF5 1.12 (with the
// `_2` suffix). bridge to the legacy spellings on 1.10 (Ubuntu 24.04 ships
// 1.10.10) so the same source builds on Trixie/Arch/Noble alike.
#if H5_VERSION_GE(1, 12, 0)
typedef H5L_info2_t wmt_h5_link_info_t;
#define WMT_H5LITERATE H5Literate2
#define WMT_H5VLEN_RECLAIM H5Treclaim
#else
typedef H5L_info_t wmt_h5_link_info_t;
#define WMT_H5LITERATE H5Literate
#define WMT_H5VLEN_RECLAIM H5Dvlen_reclaim
#endif

// returns the file id (>=0) on success, -1 on failure. libhdf5 errors silenced
// so a failed open isn't noise on stderr.
static inline int64_t wmt_h5_open_ro(const char *path) {
    int64_t out = -1;
    H5E_BEGIN_TRY {
        hid_t fid = H5Fopen(path, H5F_ACC_RDONLY, H5P_DEFAULT);
        if (fid >= 0) out = (int64_t)fid;
    } H5E_END_TRY
    return out;
}

static inline void wmt_h5_close_file(int64_t fid) {
    if (fid >= 0) H5Fclose((hid_t)fid);
}

static inline int wmt_h5_link_exists(int64_t loc, const char *path) {
    int rc = 0;
    H5E_BEGIN_TRY {
        if (H5Lexists((hid_t)loc, path, H5P_DEFAULT) > 0) {
            hid_t obj = H5Oopen((hid_t)loc, path, H5P_DEFAULT);
            if (obj >= 0) {
                H5Oclose(obj);
                rc = 1;
            }
        }
    } H5E_END_TRY
    return rc;
}

// resolves obj_path under loc; returns loc itself for NULL/empty path so the
// attr/dataset helpers work uniformly on the file root and on subgroups.
static inline hid_t wmt_h5_open_obj_(int64_t loc, const char *obj_path) {
    if (obj_path == NULL || obj_path[0] == '\0') return (hid_t)loc;
    return H5Oopen((hid_t)loc, obj_path, H5P_DEFAULT);
}

static inline void wmt_h5_release_obj_(hid_t obj, int64_t loc) {
    if (obj >= 0 && obj != (hid_t)loc) H5Oclose(obj);
}

static inline int wmt_h5_attr_str(int64_t loc, const char *obj_path,
                                   const char *attr_name, char *out, size_t cap) {
    hid_t obj = -1, attr = -1, atype = -1, mtype = -1, aspace = -1;
    int rc = -1;
    htri_t is_vlen;
    char *vbuf = NULL;
    char *fbuf = NULL;
    size_t sz = 0;
    size_t n = 0;

    if (cap == 0) return -1;
    out[0] = '\0';

    H5E_BEGIN_TRY {
        obj = wmt_h5_open_obj_(loc, obj_path);
        if (obj < 0) goto done;
        attr = H5Aopen(obj, attr_name, H5P_DEFAULT);
        if (attr < 0) goto done;
        atype = H5Aget_type(attr);
        if (atype < 0) goto done;
        if (H5Tget_class(atype) != H5T_STRING) goto done;
        is_vlen = H5Tis_variable_str(atype);
        if (is_vlen > 0) {
            mtype = H5Tcopy(H5T_C_S1);
            H5Tset_size(mtype, H5T_VARIABLE);
            if (H5Aread(attr, mtype, &vbuf) >= 0 && vbuf != NULL) {
                n = strlen(vbuf);
                if (n >= cap) n = cap - 1;
                memcpy(out, vbuf, n);
                out[n] = '\0';
                rc = 0;
                aspace = H5Aget_space(attr);
                WMT_H5VLEN_RECLAIM(mtype, aspace, H5P_DEFAULT, &vbuf);
            }
        } else {
            sz = H5Tget_size(atype);
            if (sz == 0) sz = 1;
            fbuf = (char *)calloc(sz + 1, 1);
            if (fbuf && H5Aread(attr, atype, fbuf) >= 0) {
                n = strnlen(fbuf, sz);
                if (n >= cap) n = cap - 1;
                memcpy(out, fbuf, n);
                out[n] = '\0';
                rc = 0;
            }
        }
    done:
        if (fbuf) free(fbuf);
        if (aspace >= 0) H5Sclose(aspace);
        if (mtype >= 0) H5Tclose(mtype);
        if (atype >= 0) H5Tclose(atype);
        if (attr >= 0) H5Aclose(attr);
        wmt_h5_release_obj_(obj, loc);
    } H5E_END_TRY
    return rc;
}

static inline int wmt_h5_attr_f64(int64_t loc, const char *obj_path,
                                   const char *attr_name, double *out) {
    hid_t obj = -1, attr = -1;
    int rc = -1;

    H5E_BEGIN_TRY {
        obj = wmt_h5_open_obj_(loc, obj_path);
        if (obj < 0) goto done;
        attr = H5Aopen(obj, attr_name, H5P_DEFAULT);
        if (attr < 0) goto done;
        if (H5Aread(attr, H5T_NATIVE_DOUBLE, out) >= 0) rc = 0;
    done:
        if (attr >= 0) H5Aclose(attr);
        wmt_h5_release_obj_(obj, loc);
    } H5E_END_TRY
    return rc;
}

static inline int wmt_h5_attr_i64(int64_t loc, const char *obj_path,
                                   const char *attr_name, int64_t *out) {
    hid_t obj = -1, attr = -1;
    int rc = -1;
    long long v = 0;

    H5E_BEGIN_TRY {
        obj = wmt_h5_open_obj_(loc, obj_path);
        if (obj < 0) goto done;
        attr = H5Aopen(obj, attr_name, H5P_DEFAULT);
        if (attr < 0) goto done;
        if (H5Aread(attr, H5T_NATIVE_LLONG, &v) >= 0) {
            *out = (int64_t)v;
            rc = 0;
        }
    done:
        if (attr >= 0) H5Aclose(attr);
        wmt_h5_release_obj_(obj, loc);
    } H5E_END_TRY
    return rc;
}

static inline int wmt_h5_dataset_shape(int64_t loc, const char *path,
                                        int *ndims_out, hsize_t *dims, int max_rank) {
    hid_t dset = -1, space = -1;
    int rc = -1;
    int rank = 0;

    *ndims_out = 0;
    H5E_BEGIN_TRY {
        dset = H5Dopen2((hid_t)loc, path, H5P_DEFAULT);
        if (dset < 0) goto done;
        space = H5Dget_space(dset);
        if (space < 0) goto done;
        rank = H5Sget_simple_extent_ndims(space);
        if (rank < 0 || rank > max_rank) goto done;
        H5Sget_simple_extent_dims(space, dims, NULL);
        *ndims_out = rank;
        rc = 0;
    done:
        if (space >= 0) H5Sclose(space);
        if (dset >= 0) H5Dclose(dset);
    } H5E_END_TRY
    return rc;
}

// libhdf5 handles decompression and type conversion transparently here, which
// is why we don't expose chunking or filter pipelines to Go.
static inline int wmt_h5_read_u16(int64_t loc, const char *path,
                                   uint16_t *buf, size_t cap_elems) {
    hid_t dset = -1, space = -1;
    int rc = -1;
    hssize_t n = 0;

    H5E_BEGIN_TRY {
        dset = H5Dopen2((hid_t)loc, path, H5P_DEFAULT);
        if (dset < 0) goto done;
        space = H5Dget_space(dset);
        if (space < 0) goto done;
        n = H5Sget_simple_extent_npoints(space);
        if (n < 0 || (size_t)n > cap_elems) goto done;
        if (H5Dread(dset, H5T_NATIVE_USHORT, H5S_ALL, H5S_ALL, H5P_DEFAULT, buf) >= 0) {
            rc = 0;
        }
    done:
        if (space >= 0) H5Sclose(space);
        if (dset >= 0) H5Dclose(dset);
    } H5E_END_TRY
    return rc;
}

static inline int wmt_h5_read_f32(int64_t loc, const char *path,
                                   float *buf, size_t cap_elems) {
    hid_t dset = -1, space = -1;
    int rc = -1;
    hssize_t n = 0;

    H5E_BEGIN_TRY {
        dset = H5Dopen2((hid_t)loc, path, H5P_DEFAULT);
        if (dset < 0) goto done;
        space = H5Dget_space(dset);
        if (space < 0) goto done;
        n = H5Sget_simple_extent_npoints(space);
        if (n < 0 || (size_t)n > cap_elems) goto done;
        if (H5Dread(dset, H5T_NATIVE_FLOAT, H5S_ALL, H5S_ALL, H5P_DEFAULT, buf) >= 0) {
            rc = 0;
        }
    done:
        if (space >= 0) H5Sclose(space);
        if (dset >= 0) H5Dclose(dset);
    } H5E_END_TRY
    return rc;
}

static inline int wmt_h5_read_f64(int64_t loc, const char *path,
                                   double *buf, size_t cap_elems) {
    hid_t dset = -1, space = -1;
    int rc = -1;
    hssize_t n = 0;

    H5E_BEGIN_TRY {
        dset = H5Dopen2((hid_t)loc, path, H5P_DEFAULT);
        if (dset < 0) goto done;
        space = H5Dget_space(dset);
        if (space < 0) goto done;
        n = H5Sget_simple_extent_npoints(space);
        if (n < 0 || (size_t)n > cap_elems) goto done;
        if (H5Dread(dset, H5T_NATIVE_DOUBLE, H5S_ALL, H5S_ALL, H5P_DEFAULT, buf) >= 0) {
            rc = 0;
        }
    done:
        if (space >= 0) H5Sclose(space);
        if (dset >= 0) H5Dclose(dset);
    } H5E_END_TRY
    return rc;
}

// returns subgroup names as NUL-terminated strings packed back-to-back. one C
// call instead of an N-callback dance keeps the cgo crossing cheap.
typedef struct wmt_h5_link_iter_state {
    char *out;
    size_t cap;
    size_t used;
    size_t count;
    size_t max;
} wmt_h5_link_iter_state_t;

static inline herr_t wmt_h5_collect_link_(hid_t group, const char *name,
                                          const wmt_h5_link_info_t *info, void *op_data) {
    wmt_h5_link_iter_state_t *st;
    size_t n;
    (void)group; (void)info;
    st = (wmt_h5_link_iter_state_t *)op_data;
    if (st->count >= st->max) return 1;
    n = strlen(name);
    if (st->used + n + 1 > st->cap) return 0;
    memcpy(st->out + st->used, name, n);
    st->out[st->used + n] = '\0';
    st->used += n + 1;
    st->count++;
    return 0;
}

static inline int wmt_h5_list_links(int64_t loc, const char *path,
                                     char *out, size_t cap,
                                     size_t *bytes_used, size_t *count,
                                     size_t max_names) {
    hid_t obj = -1;
    int rc = -1;
    wmt_h5_link_iter_state_t st;
    hsize_t idx = 0;

    *bytes_used = 0;
    *count = 0;
    st.out = out;
    st.cap = cap;
    st.used = 0;
    st.count = 0;
    st.max = max_names;

    H5E_BEGIN_TRY {
        obj = wmt_h5_open_obj_(loc, path);
        if (obj < 0) goto done;
        WMT_H5LITERATE(obj, H5_INDEX_NAME, H5_ITER_INC, &idx,
                       wmt_h5_collect_link_, &st);
        *bytes_used = st.used;
        *count = st.count;
        rc = 0;
    done:
        wmt_h5_release_obj_(obj, loc);
    } H5E_END_TRY
    return rc;
}

#endif // WMT_HDF5_H
