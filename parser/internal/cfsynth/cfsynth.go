// Package cfsynth writes synthetic CF-1.7 HDF5 fixtures for parser tests. It's
// a separate cgo package because Go disallows cgo inside _test.go files of a
// package that itself uses cgo.
package cfsynth

/*
#cgo pkg-config: hdf5
#include <hdf5.h>
#include <stdlib.h>
#include <string.h>

static int wmt_test_write_cf(const char *path,
                              const double *lats, int nlat,
                              const double *lons, int nlon,
                              const double *times, int ntime,
                              const float *vals, int nvals,
                              float fill_value,
                              float scale_factor,
                              float add_offset) {
    if (nvals != ntime * nlat * nlon) return -1;
    hid_t fid = H5Fcreate(path, H5F_ACC_TRUNC, H5P_DEFAULT, H5P_DEFAULT);
    if (fid < 0) return -1;

    {
        hid_t s = H5Tcopy(H5T_C_S1);
        H5Tset_size(s, 6);
        hid_t sp = H5Screate(H5S_SCALAR);
        hid_t a = H5Acreate2(fid, "Conventions", s, sp, H5P_DEFAULT, H5P_DEFAULT);
        H5Awrite(a, s, "CF-1.7");
        H5Aclose(a); H5Sclose(sp); H5Tclose(s);
    }

    #define WRITE_F64_1D(NAME, BUF, N)                                      \
        do {                                                                 \
            hsize_t d[1] = {(hsize_t)(N)};                                   \
            hid_t sp = H5Screate_simple(1, d, NULL);                         \
            hid_t ds = H5Dcreate2(fid, NAME, H5T_IEEE_F64LE, sp,             \
                                  H5P_DEFAULT, H5P_DEFAULT, H5P_DEFAULT);   \
            H5Dwrite(ds, H5T_NATIVE_DOUBLE, H5S_ALL, H5S_ALL,                \
                     H5P_DEFAULT, BUF);                                      \
            H5Dclose(ds); H5Sclose(sp);                                      \
        } while (0)

    WRITE_F64_1D("/lat", lats, nlat);
    WRITE_F64_1D("/lon", lons, nlon);
    WRITE_F64_1D("/time", times, ntime);

    {
        hid_t obj = H5Oopen(fid, "/time", H5P_DEFAULT);
        const char *u = "seconds since 2026-01-01 00:00:00";
        hid_t st = H5Tcopy(H5T_C_S1);
        H5Tset_size(st, strlen(u));
        hid_t sp = H5Screate(H5S_SCALAR);
        hid_t a = H5Acreate2(obj, "units", st, sp, H5P_DEFAULT, H5P_DEFAULT);
        H5Awrite(a, st, u);
        H5Aclose(a); H5Sclose(sp); H5Tclose(st);
        H5Oclose(obj);
    }

    {
        hsize_t dims[3] = {(hsize_t)ntime, (hsize_t)nlat, (hsize_t)nlon};
        hid_t sp = H5Screate_simple(3, dims, NULL);
        hid_t ds = H5Dcreate2(fid, "/t2m", H5T_IEEE_F32LE, sp,
                              H5P_DEFAULT, H5P_DEFAULT, H5P_DEFAULT);
        H5Dwrite(ds, H5T_NATIVE_FLOAT, H5S_ALL, H5S_ALL, H5P_DEFAULT, vals);

        #define WRITE_STR_ATTR(OBJ, NAME, VAL)                              \
            do {                                                             \
                const char *vv = VAL;                                        \
                hid_t st = H5Tcopy(H5T_C_S1);                                \
                H5Tset_size(st, strlen(vv));                                 \
                hid_t asp = H5Screate(H5S_SCALAR);                           \
                hid_t a = H5Acreate2(OBJ, NAME, st, asp, H5P_DEFAULT, H5P_DEFAULT); \
                H5Awrite(a, st, vv);                                         \
                H5Aclose(a); H5Sclose(asp); H5Tclose(st);                    \
            } while (0)
        #define WRITE_F32_SCALAR_ATTR(OBJ, NAME, VAL)                       \
            do {                                                             \
                float vv = VAL;                                              \
                hid_t asp = H5Screate(H5S_SCALAR);                           \
                hid_t a = H5Acreate2(OBJ, NAME, H5T_IEEE_F32LE, asp, H5P_DEFAULT, H5P_DEFAULT); \
                H5Awrite(a, H5T_NATIVE_FLOAT, &vv);                          \
                H5Aclose(a); H5Sclose(asp);                                  \
            } while (0)

        WRITE_STR_ATTR(ds, "units", "K");
        WRITE_STR_ATTR(ds, "standard_name", "air_temperature");
        WRITE_STR_ATTR(ds, "long_name", "2 metre temperature");
        WRITE_F32_SCALAR_ATTR(ds, "_FillValue", fill_value);
        WRITE_F32_SCALAR_ATTR(ds, "scale_factor", scale_factor);
        WRITE_F32_SCALAR_ATTR(ds, "add_offset", add_offset);

        H5Dclose(ds); H5Sclose(sp);
    }

    H5Fclose(fid);
    return 0;
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// WriteCFFile writes a minimal CF-1.7 file: one f32 t2m(time, lat, lon)
// variable plus 1D lat/lon/time coords with the standard CF attributes.
func WriteCFFile(path string,
	lats, lons, times []float64,
	vals []float32,
	fill, scale, offset float32) error {
	cp := C.CString(path)
	defer C.free(unsafe.Pointer(cp))
	if len(vals) != len(times)*len(lats)*len(lons) {
		return fmt.Errorf("cfsynth: vals len %d != %d*%d*%d", len(vals), len(times), len(lats), len(lons))
	}
	rc := C.wmt_test_write_cf(cp,
		(*C.double)(unsafe.Pointer(&lats[0])), C.int(len(lats)),
		(*C.double)(unsafe.Pointer(&lons[0])), C.int(len(lons)),
		(*C.double)(unsafe.Pointer(&times[0])), C.int(len(times)),
		(*C.float)(unsafe.Pointer(&vals[0])), C.int(len(vals)),
		C.float(fill), C.float(scale), C.float(offset))
	if rc != 0 {
		return fmt.Errorf("cfsynth: H5Fcreate/write returned %d", int(rc))
	}
	return nil
}
