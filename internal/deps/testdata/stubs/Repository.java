// Signatures read from lib-1.0.jar by Java Support Lite; no sources are attached.
package com.example.lib;

import java.util.List;

public interface Repository<T, ID extends Comparable<ID>> extends Iterable<T> {
    T findById(ID arg0);
    List<T> findAll(String... arg0);
    default boolean exists(ID id) { /* compiled code */ }
    static <R> R identity(R r) { /* compiled code */ }
}
