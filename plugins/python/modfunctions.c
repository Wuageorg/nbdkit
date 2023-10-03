/* nbdkit
 * Copyright Red Hat
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

/* Functions and constants in the virtual nbdkit.* module. */

#include <config.h>

#include <stdio.h>
#include <stdlib.h>

#include "plugin.h"

__thread int last_error;

/* nbdkit.debug */
static PyObject *
debug (PyObject *self, PyObject *args)
{
  const char *msg;

  if (!PyArg_ParseTuple (args, "s:debug", &msg))
    return NULL;
  nbdkit_debug ("%s", msg);
  Py_RETURN_NONE;
}

/* nbdkit.export_name */
static PyObject *
export_name (PyObject *self, PyObject *args)
{
  const char *s = nbdkit_export_name ();

  if (!s) {
    /* Unfortunately we lose the actual error. XXX */
    PyErr_SetString (PyExc_RuntimeError, "nbdkit.export_name failed");
    return NULL;
  }

  /* NBD spec says that the export name should be UTF-8, so this
   * ought to work, and if it fails the client gave us a bad export
   * name which should turn into an exception.
   */
  return PyUnicode_FromString (s);
}

/* nbdkit.set_error */
static PyObject *
set_error (PyObject *self, PyObject *args)
{
  int err;

  if (!PyArg_ParseTuple (args, "i:set_error", &err))
    return NULL;
  nbdkit_set_error (err);
  last_error = err;
  Py_RETURN_NONE;
}

/* nbdkit.shutdown */
static PyObject *
do_shutdown (PyObject *self, PyObject *args)
{
  nbdkit_shutdown ();
  Py_RETURN_NONE;
}

/* nbdkit.disconnect */
static PyObject *
do_disconnect (PyObject *self, PyObject *args)
{
  int force;

  if (!PyArg_ParseTuple (args, "p:disconnect", &force))
    return NULL;
  nbdkit_disconnect (force);
  Py_RETURN_NONE;
}

/* nbdkit.parse_size */
static PyObject *
parse_size (PyObject *self, PyObject *args)
{
  const char *s;
  if (!PyArg_ParseTuple (args, "s:parse_size", &s))
    return NULL;

  int64_t size = nbdkit_parse_size (s);
  if (size == -1) {
    PyErr_SetString (PyExc_ValueError, "Unable to parse string as size");
    return NULL;
  }

  return PyLong_FromSize_t ((size_t)size);
}

/* nbdkit.parse_probability */
static PyObject *
parse_probability (PyObject *self, PyObject *args)
{
  const char *what, *str;
  double d;

  if (!PyArg_ParseTuple (args, "ss:parse_probability", &what, &str))
    return NULL;

  if (nbdkit_parse_probability (what, str, &d) == -1) {
    PyErr_SetString (PyExc_ValueError,
                     "Unable to parse string as probability");
    return NULL;
  }

  return PyFloat_FromDouble (d);
}

/* nbdkit.stdio_safe */
static PyObject *
do_stdio_safe (PyObject *self, PyObject *args)
{
  int b = nbdkit_stdio_safe ();
  if (b) Py_RETURN_TRUE; else Py_RETURN_FALSE;
}

/* nbdkit.is_tls */
static PyObject *
do_is_tls (PyObject *self, PyObject *args)
{
  int b = nbdkit_is_tls ();
  if (b) Py_RETURN_TRUE; else Py_RETURN_FALSE;
}

/* nbdkit.nanosleep */
static PyObject *
do_nanosleep (PyObject *self, PyObject *args)
{
  unsigned secs, nsecs;

  if (!PyArg_ParseTuple (args, "II:nanosleep", &secs, &nsecs))
    return NULL;
  /* This can return an error but in this case it is probably best to
   * ignore it since it most probably indicates that the plugin is
   * exiting.
   */
  nbdkit_nanosleep (secs, nsecs);
  Py_RETURN_NONE;
}

static PyMethodDef NbdkitMethods[] = {
  { "debug", debug, METH_VARARGS,
    "Print a debug message" },
  { "disconnect", do_disconnect, METH_VARARGS,
    "Request disconnection from current client" },
  { "export_name", export_name, METH_NOARGS,
    "Return the optional export name negotiated with the client" },
  { "is_tls", do_is_tls, METH_NOARGS,
    "Return True if the client completed TLS authentication" },
  { "nanosleep", do_nanosleep, METH_VARARGS,
    "Sleep for seconds and nanoseconds" },
  { "parse_probability", parse_probability, METH_VARARGS,
    "Parse probability strings into floating point number" },
  { "parse_size", parse_size, METH_VARARGS,
    "Parse human-readable size strings into bytes" },
  { "set_error", set_error, METH_VARARGS,
    "Store an errno value prior to throwing an exception" },
  { "shutdown", do_shutdown, METH_NOARGS,
    "Request asynchronous shutdown" },
  { "stdio_safe", do_stdio_safe, METH_NOARGS,
    "Return True if it is safe to interact with stdin and stdout" },
  { NULL }
};

static struct PyModuleDef moduledef = {
  PyModuleDef_HEAD_INIT,
  "nbdkit",
  "Module used to access nbdkit server API",
  -1,
  NbdkitMethods,
  NULL,
  NULL,
  NULL,
  NULL
};

PyMODINIT_FUNC
create_nbdkit_module (void)
{
  PyObject *m;

  m = PyModule_Create (&moduledef);
  if (m == NULL) {
    nbdkit_error ("could not create the nbdkit API module");
    exit (EXIT_FAILURE);
  }

  /* Constants corresponding to various flags. */
#define ADD_INT_CONSTANT(name)                                      \
  if (PyModule_AddIntConstant (m, #name, NBDKIT_##name) == -1) {    \
    nbdkit_error ("could not add constant %s to nbdkit API module", \
                  #name);                                           \
    exit (EXIT_FAILURE);                                            \
  }
  ADD_INT_CONSTANT (THREAD_MODEL_SERIALIZE_CONNECTIONS);
  ADD_INT_CONSTANT (THREAD_MODEL_SERIALIZE_ALL_REQUESTS);
  ADD_INT_CONSTANT (THREAD_MODEL_SERIALIZE_REQUESTS);
  ADD_INT_CONSTANT (THREAD_MODEL_PARALLEL);

  ADD_INT_CONSTANT (FLAG_MAY_TRIM);
  ADD_INT_CONSTANT (FLAG_FUA);
  ADD_INT_CONSTANT (FLAG_REQ_ONE);
  ADD_INT_CONSTANT (FLAG_FAST_ZERO);

  ADD_INT_CONSTANT (FUA_NONE);
  ADD_INT_CONSTANT (FUA_EMULATE);
  ADD_INT_CONSTANT (FUA_NATIVE);

  ADD_INT_CONSTANT (CACHE_NONE);
  ADD_INT_CONSTANT (CACHE_EMULATE);
  ADD_INT_CONSTANT (CACHE_NATIVE);

  ADD_INT_CONSTANT (EXTENT_HOLE);
  ADD_INT_CONSTANT (EXTENT_ZERO);
#undef ADD_INT_CONSTANT

  return m;
}
