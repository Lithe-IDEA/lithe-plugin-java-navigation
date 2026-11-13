package com.example.lib;

import java.util.List;

public interface Repository<T, ID extends Comparable<ID>> extends Iterable<T> {
    T findById(ID id);

    List<T> findAll(String... names);

    default boolean exists(ID id) {
        return findById(id) != null;
    }

    static <R> R identity(R r) {
        return r;
    }
}
