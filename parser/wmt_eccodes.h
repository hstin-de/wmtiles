// Internal cgo helpers shared across this package's preludes.
#ifndef WMT_ECCODES_H
#define WMT_ECCODES_H

#include "eccodes.h"
#include <stddef.h>
#include <stdlib.h>
#include <string.h>

// codes_get_float_array landed in eccodes 2.30.0. Redeclare the prototype as
// weak so we can take its address even when the installed eccodes header omits
// it; &codes_get_float_array resolves to NULL at link time on older releases
// and we fall back to double + narrow.
#if defined(__GNUC__) || defined(__clang__)
extern int codes_get_float_array(const codes_handle *h, const char *key,
                                 float *vals, size_t *length)
    __attribute__((weak));
#  define WMT_HAVE_WEAK_SYMBOLS 1
#endif

static inline int wmt_get_float_array(codes_handle *h, const char *key,
                                      float *vals, size_t *len_inout) {
#ifdef WMT_HAVE_WEAK_SYMBOLS
    if (codes_get_float_array != NULL) {
        return codes_get_float_array(h, key, vals, len_inout);
    }
#endif
    if (vals == NULL || len_inout == NULL || *len_inout == 0) {
        return CODES_INVALID_ARGUMENT;
    }
    size_t n = *len_inout;
    double *tmp = (double *)malloc(n * sizeof(double));
    if (tmp == NULL) return CODES_OUT_OF_MEMORY;
    int rc = codes_get_double_array(h, key, tmp, &n);
    if (rc == CODES_SUCCESS) {
        for (size_t i = 0; i < n; i++) vals[i] = (float)tmp[i];
        *len_inout = n;
    }
    free(tmp);
    return rc;
}

typedef struct wmt_scalars {
    long ni, nj;
    double la1, la2, lo1, lo2, dx, dy;
    double basicAngle, subdivisions, missingValue;
    long year, month, day, hour, minute, second;
    long timeUnit, forecastTime;
    long discipline, parameterCategory, parameterNumber;
    long level, bottomLevel;
    char shortName[64];
    char units[64];
    char typeOfLevel[64];
} wmt_scalars_t;

static inline void wmt_glong(codes_handle *h, const char *k, long *v) {
    codes_get_long(h, k, v);
}
static inline void wmt_gdouble(codes_handle *h, const char *k, double *v) {
    codes_get_double(h, k, v);
}
static inline void wmt_gstring(codes_handle *h, const char *k, char *out, size_t cap) {
    size_t n = cap;
    if (codes_get_string(h, k, out, &n) != CODES_SUCCESS) {
        out[0] = '\0';
    }
}

static inline void wmt_read_scalars(codes_handle *h, wmt_scalars_t *o) {
    wmt_glong(h, "Ni", &o->ni);
    wmt_glong(h, "Nj", &o->nj);
    wmt_gdouble(h, "latitudeOfFirstGridPointInDegrees", &o->la1);
    wmt_gdouble(h, "latitudeOfLastGridPointInDegrees",  &o->la2);
    wmt_gdouble(h, "longitudeOfFirstGridPointInDegrees", &o->lo1);
    wmt_gdouble(h, "longitudeOfLastGridPointInDegrees",  &o->lo2);
    wmt_gdouble(h, "iDirectionIncrement", &o->dx);
    wmt_gdouble(h, "jDirectionIncrement", &o->dy);
    wmt_gdouble(h, "basicAngleOfTheInitialProductionDomain", &o->basicAngle);
    wmt_gdouble(h, "subdivisionsOfBasicAngle", &o->subdivisions);
    wmt_gdouble(h, "missingValue", &o->missingValue);
    wmt_glong(h, "year",   &o->year);
    wmt_glong(h, "month",  &o->month);
    wmt_glong(h, "day",    &o->day);
    wmt_glong(h, "hour",   &o->hour);
    wmt_glong(h, "minute", &o->minute);
    wmt_glong(h, "second", &o->second);
    wmt_glong(h, "indicatorOfUnitOfTimeRange", &o->timeUnit);
    wmt_glong(h, "forecastTime", &o->forecastTime);
    wmt_glong(h, "discipline",        &o->discipline);
    wmt_glong(h, "parameterCategory", &o->parameterCategory);
    wmt_glong(h, "parameterNumber",   &o->parameterNumber);
    wmt_glong(h, "level",       &o->level);
    wmt_glong(h, "bottomLevel", &o->bottomLevel);
    wmt_gstring(h, "shortName",   o->shortName,   sizeof(o->shortName));
    wmt_gstring(h, "units",       o->units,       sizeof(o->units));
    wmt_gstring(h, "typeOfLevel", o->typeOfLevel, sizeof(o->typeOfLevel));
}

// Returns 0 on success; -1 on handle creation failure; -2 if cap_values
// is too small for ni*nj; otherwise the eccodes rc from get_float_array.
static inline int wmt_decode_full(codes_context *ctx,
                                  const void *buf, size_t buf_len,
                                  wmt_scalars_t *scalars,
                                  float *values, size_t cap_values,
                                  size_t *values_len) {
    if (ctx == NULL) ctx = codes_context_get_default();
    codes_handle *h = codes_handle_new_from_message(ctx, (void *)buf, buf_len);
    if (!h) return -1;
    wmt_read_scalars(h, scalars);
    if (cap_values == 0 || values == NULL) {
        *values_len = 0;
        codes_handle_delete(h);
        return 0;
    }
    size_t want = (size_t)scalars->ni * (size_t)scalars->nj;
    if (want == 0 || want > cap_values) {
        codes_handle_delete(h);
        return -2;
    }
    *values_len = want;
    int rc = wmt_get_float_array(h, "values", values, values_len);
    codes_handle_delete(h);
    return rc;
}

#endif // WMT_ECCODES_H
