/* nbdkit
 * Copyright (C) 2019 Red Hat Inc.
 *
 * Redistribution and use in source and binary forms, with or without
 * modification, are permitted provided that the following conditions are
 * met:
 *
 * * Redistributions of source code must retain the above copyright
 * notice, this list of conditions and the following disclaimer.
 *
 * * Redistributions in binary form must reproduce the above copyright
 * notice, this list of conditions and the following disclaimer in the
 * documentation and/or other materials provided with the distribution.
 *
 * * Neither the name of Red Hat nor the names of its contributors may be
 * used to endorse or promote products derived from this software without
 * specific prior written permission.
 *
 * THIS SOFTWARE IS PROVIDED BY RED HAT AND CONTRIBUTORS ''AS IS'' AND
 * ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO,
 * THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A
 * PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL RED HAT OR
 * CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
 * SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
 * LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF
 * USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND
 * ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
 * OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT
 * OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF
 * SUCH DAMAGE.
 */

#include <config.h>

#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>
#include <stdbool.h>
#include <inttypes.h>
#include <string.h>
#include <sys/time.h>

#include <nbdkit-filter.h>

#include "cleanup.h"

static unsigned retries = 5;    /* 0 = filter is disabled */
static unsigned initial_delay = 2;
static bool exponential_backoff = true;
static bool force_readonly = false;

/* Currently next_ops->reopen is not safe if another thread makes a
 * request on the same connection (but on other connections it's OK).
 * To work around this for now we limit the thread model here, but
 * this is something we could improve in server/backend.c in future.
 */
static int
retry_thread_model (void)
{
  return NBDKIT_THREAD_MODEL_SERIALIZE_REQUESTS;
}

static int
retry_config (nbdkit_next_config *next, void *nxdata,
              const char *key, const char *value)
{
  int r;

  if (strcmp (key, "retries") == 0) {
    if (nbdkit_parse_unsigned ("retries", value, &retries) == -1)
      return -1;
    return 0;
  }
  else if (strcmp (key, "retry-delay") == 0) {
    if (nbdkit_parse_unsigned ("retry-delay", value, &initial_delay) == -1)
      return -1;
    if (initial_delay == 0) {
      nbdkit_error ("retry-delay cannot be 0");
      return -1;
    }
    return 0;
  }
  else if (strcmp (key, "retry-exponential") == 0) {
    r = nbdkit_parse_bool (value);
    if (r == -1)
      return -1;
    exponential_backoff = r;
    return 0;
  }
  else if (strcmp (key, "retry-readonly") == 0) {
    r = nbdkit_parse_bool (value);
    if (r == -1)
      return -1;
    force_readonly = r;
    return 0;
  }

  return next (nxdata, key, value);
}

#define retry_config_help \
  "retries=<N>              Number of retries (default: 5).\n" \
  "retry-delay=<N>          Seconds to wait before retry (default: 2).\n" \
  "retry-exponential=yes|no Exponential back-off (default: yes).\n" \
  "retry-readonly=yes|no    Force read-only on failure (default: no).\n"

struct retry_handle {
  int readonly;                 /* Save original readonly setting. */
};

static void *
retry_open (nbdkit_next_open *next, void *nxdata, int readonly)
{
  struct retry_handle *h;

  if (next (nxdata, readonly) == -1)
    return NULL;

  h = malloc (sizeof *h);
  if (h == NULL) {
    nbdkit_error ("malloc: %m");
    return NULL;
  }

  h->readonly = readonly;

  return h;
}

static void
retry_close (void *handle)
{
  struct retry_handle *h = handle;

  free (h);
}

/* This function encapsulates the common retry logic used across all
 * data commands.  If it returns true then the data command will retry
 * the operation.  ‘struct retry_data’ is stack data saved between
 * retries within the same command, and is initialized to zero.
 */
struct retry_data {
  int retry;                    /* Retry number (0 = first time). */
  int delay;                    /* Seconds to wait before retrying. */
};

static bool
do_retry (struct retry_handle *h,
          struct retry_data *data,
          struct nbdkit_next_ops *next_ops, void *nxdata,
          int *err)
{
  /* If it's the first retry, initialize the other fields in *data. */
  if (data->retry == 0)
    data->delay = initial_delay;

 again:
  /* Log the original errno since it will be lost when we retry. */
  nbdkit_debug ("retry %d: original errno = %d", data->retry+1, *err);

  if (data->retry >= retries) {
    nbdkit_debug ("could not recover after %d retries", retries);
    return false;
  }

  nbdkit_debug ("waiting %d seconds before retrying", data->delay);
  if (nbdkit_nanosleep (data->delay, 0) == -1) {
    /* We could do this but it would overwrite the more important
     * errno from the underlying data call.
     */
    /* *err = errno; */
    return false;
  }

  /* Update *data in case we are called again. */
  data->retry++;
  if (exponential_backoff)
    data->delay *= 2;

  /* Reopen the connection. */
  if (next_ops->reopen (nxdata, h->readonly || force_readonly) == -1) {
    /* If the reopen fails we treat it the same way as a command
     * failing.
     */
    goto again;
  }

  /* Retry the data command. */
  return true;
}

static int
retry_pread (struct nbdkit_next_ops *next_ops, void *nxdata,
             void *handle, void *buf, uint32_t count, uint64_t offset,
             uint32_t flags, int *err)
{
  struct retry_handle *h = handle;
  struct retry_data data = {0};
  int r;

 again:
  r = next_ops->pread (nxdata, buf, count, offset, flags, err);
  if (r == -1 && do_retry (h, &data, next_ops, nxdata, err)) goto again;

  return r;
}

/* Write. */
static int
retry_pwrite (struct nbdkit_next_ops *next_ops, void *nxdata,
              void *handle,
              const void *buf, uint32_t count, uint64_t offset,
              uint32_t flags, int *err)
{
  struct retry_handle *h = handle;
  struct retry_data data = {0};
  int r;

 again:
  r = next_ops->pwrite (nxdata, buf, count, offset, flags, err);
  if (r == -1 && do_retry (h, &data, next_ops, nxdata, err)) goto again;

  return r;
}

/* Trim. */
static int
retry_trim (struct nbdkit_next_ops *next_ops, void *nxdata,
            void *handle,
            uint32_t count, uint64_t offset, uint32_t flags,
            int *err)
{
  struct retry_handle *h = handle;
  struct retry_data data = {0};
  int r;

 again:
  r = next_ops->trim (nxdata, count, offset, flags, err);
  if (r == -1 && do_retry (h, &data, next_ops, nxdata, err)) goto again;

  return r;
}

/* Flush. */
static int
retry_flush (struct nbdkit_next_ops *next_ops, void *nxdata,
             void *handle, uint32_t flags,
             int *err)
{
  struct retry_handle *h = handle;
  struct retry_data data = {0};
  int r;

 again:
  r = next_ops->flush (nxdata, flags, err);
  if (r == -1 && do_retry (h, &data, next_ops, nxdata, err)) goto again;

  return r;
}

/* Zero. */
static int
retry_zero (struct nbdkit_next_ops *next_ops, void *nxdata,
            void *handle,
            uint32_t count, uint64_t offset, uint32_t flags,
            int *err)
{
  struct retry_handle *h = handle;
  struct retry_data data = {0};
  int r;

 again:
  r = next_ops->zero (nxdata, count, offset, flags, err);
  if (r == -1 && do_retry (h, &data, next_ops, nxdata, err)) goto again;

  return r;
}

/* Extents. */
static int
retry_extents (struct nbdkit_next_ops *next_ops, void *nxdata,
               void *handle,
               uint32_t count, uint64_t offset, uint32_t flags,
               struct nbdkit_extents *extents, int *err)
{
  struct retry_handle *h = handle;
  struct retry_data data = {0};
  int r;

 again:
  r = next_ops->extents (nxdata, count, offset, flags, extents, err);
  if (r == -1 && do_retry (h, &data, next_ops, nxdata, err)) goto again;

  return r;
}

/* Cache. */
static int
retry_cache (struct nbdkit_next_ops *next_ops, void *nxdata,
             void *handle,
             uint32_t count, uint64_t offset, uint32_t flags,
             int *err)
{
  struct retry_handle *h = handle;
  struct retry_data data = {0};
  int r;

 again:
  r = next_ops->cache (nxdata, count, offset, flags, err);
  if (r == -1 && do_retry (h, &data, next_ops, nxdata, err)) goto again;

  return r;
}

static struct nbdkit_filter filter = {
  .name              = "retry",
  .longname          = "nbdkit retry filter",
  .thread_model      = retry_thread_model,
  .config            = retry_config,
  .config_help       = retry_config_help,
  .open              = retry_open,
  .close             = retry_close,
  .pread             = retry_pread,
  .pwrite            = retry_pwrite,
  .trim              = retry_trim,
  .flush             = retry_flush,
  .zero              = retry_zero,
  .extents           = retry_extents,
  .cache             = retry_cache,
};

NBDKIT_REGISTER_FILTER(filter)
