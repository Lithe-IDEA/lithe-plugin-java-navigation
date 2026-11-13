package com.example.lib;

public record Point(int x, int y) implements Comparable<Point> {
    public static final Point ORIGIN = new Point(0, 0);

    public double length() {
        return Math.hypot(x, y);
    }

    @Override
    public int compareTo(Point o) {
        return Integer.compare(x, o.x);
    }
}
